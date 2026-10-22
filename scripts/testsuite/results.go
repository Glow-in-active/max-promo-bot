//go:build tools

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type testEvent struct {
	Action     string
	Package    string
	Test       string
	Output     string
	ImportPath string
}

type packageResults struct {
	finished    bool
	noTestFiles bool
	tests       map[string]bool
}

type testResults struct {
	packages map[string]*packageResults
	tests    int
	problems []error
}

func checkResults(input io.Reader, output io.Writer) error {
	results := testResults{packages: make(map[string]*packageResults)}
	decoder := json.NewDecoder(input)
	for {
		var event testEvent
		if err := decoder.Decode(&event); err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("invalid test results: %w", err)
			}
			break
		}
		if _, err := io.WriteString(output, event.Output); err != nil {
			return fmt.Errorf("write test output: %w", err)
		}
		if err := results.record(event); err != nil {
			results.problems = append(results.problems, err)
		}
	}
	return results.complete()
}

func (r *testResults) record(event testEvent) error {
	if event.Action == "build-output" || event.Action == "build-fail" {
		if event.ImportPath == "" {
			return errors.New("build event has no import path")
		}
		if event.Action == "build-fail" {
			return fmt.Errorf("build failed: %s", event.ImportPath)
		}
		return nil
	}
	if event.Package == "" {
		return errors.New("test event has no package")
	}
	if event.Action == "start" {
		if _, exists := r.packages[event.Package]; exists || event.Test != "" {
			return fmt.Errorf("unexpected package start: %s", event.Package)
		}
		r.packages[event.Package] = &packageResults{tests: make(map[string]bool)}
		return nil
	}
	result, exists := r.packages[event.Package]
	if !exists || result.finished {
		return fmt.Errorf("event outside package run: %s", event.Package)
	}
	if event.Action == "output" {
		result.noTestFiles = result.noTestFiles || noTestFiles(event)
		return nil
	}
	if event.Test == "" {
		return r.finishPackage(event, result)
	}
	return r.recordTest(event, result)
}

func noTestFiles(event testEvent) bool {
	if event.Test != "" {
		return false
	}
	return event.Output == "?   \t"+event.Package+"\t[no test files]\n" ||
		event.Output == "\t"+event.Package+"\t\tcoverage: 0.0% of statements\n"
}

func (r *testResults) recordTest(event testEvent, result *packageResults) error {
	active := result.tests[event.Test]
	switch event.Action {
	case "run":
		if active {
			return fmt.Errorf("test started twice: %s/%s", event.Package, event.Test)
		}
		result.tests[event.Test] = true
		r.tests++
	case "pause", "cont", "bench":
		if !active {
			return fmt.Errorf("inactive test: %s/%s", event.Package, event.Test)
		}
	case "pass", "fail", "skip":
		if !active {
			return fmt.Errorf("test result without run: %s/%s", event.Package, event.Test)
		}
		result.tests[event.Test] = false
		if event.Action != "pass" {
			return fmt.Errorf("test %s: %s/%s", event.Action, event.Package, event.Test)
		}
	default:
		return fmt.Errorf("unknown test action: %q", event.Action)
	}
	return nil
}

func (r *testResults) finishPackage(event testEvent, result *packageResults) error {
	switch event.Action {
	case "pass", "fail", "skip":
		result.finished = true
		if event.Action == "fail" {
			return fmt.Errorf("package failed: %s", event.Package)
		}
		if event.Action == "skip" && (!result.noTestFiles || len(result.tests) != 0) {
			return fmt.Errorf("package skipped: %s", event.Package)
		}
		if len(result.tests) == 0 && !result.noTestFiles {
			return fmt.Errorf("no tests ran in package: %s", event.Package)
		}
	default:
		return fmt.Errorf("unexpected package action: %q", event.Action)
	}
	return nil
}

func (r *testResults) complete() error {
	if r.tests == 0 {
		r.problems = append(r.problems, errors.New("no tests ran"))
	}
	for name, result := range r.packages {
		if !result.finished {
			r.problems = append(r.problems, fmt.Errorf("package result missing: %s", name))
		}
		for test, active := range result.tests {
			if active {
				r.problems = append(r.problems, fmt.Errorf("test result missing: %s/%s", name, test))
			}
		}
	}
	return errors.Join(r.problems...)
}
