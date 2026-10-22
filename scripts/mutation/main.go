//go:build tools

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

type mutation struct {
	ID       string `json:"id"`
	File     string `json:"file"`
	Function string `json:"function"`
	Original string `json:"original"`
	Changed  string `json:"changed"`
	Package  string `json:"package"`
	Tests    string `json:"tests"`
}

type outcome struct {
	mutation
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type options struct {
	integration bool
	list        bool
	validate    bool
}

func parseOptions(args []string) (options, error) {
	var options options
	flags := flag.NewFlagSet("mutate", flag.ContinueOnError)
	flags.BoolVar(&options.integration, "integration", false, "Run cross-layer regressions against TEST_DATABASE_URL")
	flags.BoolVar(&options.list, "list", false, "List selected mutations")
	flags.BoolVar(&options.validate, "validate", false, "Validate mutation selectors without executing tests")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, errors.New("unexpected positional arguments")
	}
	return options, nil
}

func run() error {
	options, err := parseOptions(os.Args[1:])
	if err != nil {
		return err
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	mutations, reportName := domainMutations, "mutations.json"
	if options.integration {
		mutations, reportName = integrationMutations, "integration-mutations.json"
	}
	if options.list {
		return json.NewEncoder(os.Stdout).Encode(mutations)
	}
	if options.validate {
		return validateSelectors(root, mutations)
	}
	if options.integration && os.Getenv("TEST_DATABASE_URL") == "" {
		return errors.New("integration mutations require TEST_DATABASE_URL pointing to a disposable database")
	}
	if err := checkBaselines(mutations, options.integration); err != nil {
		return err
	}
	results, err := runMutations(root, mutations, options.integration)
	if err != nil {
		return err
	}
	return writeReport(results, reportName)
}

func validateSelectors(root string, mutations []mutation) error {
	for _, m := range mutations {
		if _, err := replaceExpression(filepath.Join(root, m.File), m); err != nil {
			return err
		}
	}
	fmt.Printf("All %d mutation selectors match exactly once.\n", len(mutations))
	return nil
}

func checkBaselines(mutations []mutation, integration bool) error {
	fmt.Println("Checking the unmodified test suites...")
	baselines := map[string]bool{}
	for _, m := range mutations {
		key := m.Package + " " + m.Tests
		if baselines[key] {
			continue
		}
		output, testErr, timedOut := runTests(m, "", integration)
		if status := testOutcome(output, testErr, timedOut, m.Tests); status != "survived" {
			return fmt.Errorf("baseline %s failed (%s); no mutation score was computed:\n%s", key, status, output)
		}
		baselines[key] = true
		fmt.Printf("baseline PASS %s\n", key)
	}
	return nil
}

func runMutations(root string, mutations []mutation, integration bool) ([]outcome, error) {
	temp, err := os.MkdirTemp("", "promo-mutation-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)

	results := make([]outcome, 0, len(mutations))
	for _, m := range mutations {
		result, err := runMutation(root, temp, m, integration)
		if err != nil {
			return nil, err
		}
		fmt.Printf("%-8s %s\n", result.Status, result.ID)
		results = append(results, result)
	}
	return results, nil
}

func runMutation(root, temp string, m mutation, integration bool) (outcome, error) {
	original := filepath.Join(root, m.File)
	changed, err := replaceExpression(original, m)
	if err != nil {
		return outcome{}, err
	}
	mutatedPath := filepath.Join(temp, m.ID+".go")
	if err := os.WriteFile(mutatedPath, changed, 0600); err != nil {
		return outcome{}, err
	}
	overlay, err := json.Marshal(map[string]any{
		"Replace": map[string]string{original: mutatedPath},
	})
	if err != nil {
		return outcome{}, err
	}
	overlayPath := filepath.Join(temp, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0600); err != nil {
		return outcome{}, err
	}

	output, testErr, timedOut := runTests(m, overlayPath, integration)
	result := outcome{mutation: m, Status: testOutcome(output, testErr, timedOut, m.Tests)}
	if result.Status != "killed" {
		result.Detail = string(output)
	}
	return result, nil
}

func writeReport(results []outcome, reportName string) error {
	if err := os.MkdirAll("reports", 0755); err != nil {
		return err
	}
	report, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	reportPath := filepath.Join("reports", reportName)
	if err := os.WriteFile(reportPath, append(report, '\n'), 0644); err != nil {
		return err
	}
	for _, result := range results {
		if result.Status != "killed" {
			return fmt.Errorf("mutation gate failed; inspect %s", reportPath)
		}
	}
	fmt.Printf("All %d targeted business mutations were killed. Source files were not modified.\n", len(results))
	return nil
}

func runTests(m mutation, overlay string, integration bool) ([]byte, error, bool) {
	args := []string{"test", "-json", "-count=1", "-timeout=60s", "-run=" + m.Tests}
	if integration {
		args = append(args, "-tags=integration", "-race")
	}
	if overlay != "" {
		args = append(args, "-overlay="+overlay)
	}
	args = append(args, m.Package)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	output, err := cmd.CombinedOutput()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded) || bytes.Contains(output, []byte("test timed out"))
	return output, err, timedOut
}

func testOutcome(output []byte, testErr error, timedOut bool, pattern string) string {
	if timedOut {
		return "timeout"
	}
	selector, err := regexp.Compile(pattern)
	if err != nil {
		return "invalid"
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	ran, passed, failed, skipped := false, false, false, false
	for {
		var event struct {
			Action string
			Test   string
		}
		if err := decoder.Decode(&event); err != nil {
			if !errors.Is(err, io.EOF) {
				return "invalid"
			}
			break
		}
		if event.Test == "" || !selector.MatchString(event.Test) {
			continue
		}
		ran = ran || event.Action == "run"
		passed = passed || event.Action == "pass"
		failed = failed || event.Action == "fail"
		skipped = skipped || event.Action == "skip"
	}
	if !ran || skipped {
		return "invalid"
	}
	if testErr == nil && passed && !failed {
		return "survived"
	}
	if testErr != nil && failed {
		return "killed"
	}
	return "invalid"
}

func replaceExpression(path string, m mutation) ([]byte, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, path, source, 0)
	if err != nil {
		return nil, err
	}
	var selected ast.Expr
	matches := 0
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != m.Function {
			continue
		}
		writeTargets := map[ast.Node]bool{}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if writeTargets[node] {
				return false
			}
			if assignment, ok := node.(*ast.AssignStmt); ok {
				for _, target := range assignment.Lhs {
					writeTargets[target] = true
				}
			}
			expression, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			var text bytes.Buffer
			if err := format.Node(&text, files, expression); err == nil && text.String() == m.Original {
				selected = expression
				matches++
			}
			return true
		})
	}
	if matches != 1 {
		return nil, fmt.Errorf("mutation %s expected exactly one expression in %s.%s, found %d; update the suite explicitly", m.ID, m.File, m.Function, matches)
	}
	start, end := files.Position(selected.Pos()).Offset, files.Position(selected.End()).Offset
	changed := string(source[:start]) + m.Changed + string(source[end:])
	return format.Source([]byte(changed))
}
