//go:build tools

package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestResultValidation(t *testing.T) {
	const start = `{"Action":"start","Package":"fixture"}` + "\n"
	const run = `{"Action":"run","Package":"fixture","Test":"TestRequired"}` + "\n"
	const pass = `{"Action":"pass","Package":"fixture","Test":"TestRequired"}` + "\n"
	const done = `{"Action":"pass","Package":"fixture"}` + "\n"
	const complete = start + run + pass + done
	const noFiles = `{"Action":"start","Package":"generated"}` + "\n" +
		`{"Action":"output","Package":"generated","Output":"?   \tgenerated\t[no test files]\n"}` + "\n" +
		`{"Action":"skip","Package":"generated"}` + "\n"
	const coverageNoFiles = `{"Action":"start","Package":"generated"}` + "\n" +
		`{"Action":"output","Package":"generated","Output":"\tgenerated\t\tcoverage: 0.0% of statements\n"}` + "\n" +
		`{"Action":"pass","Package":"generated"}` + "\n"

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "passed", input: complete},
		{name: "generated package", input: complete + noFiles},
		{name: "generated coverage", input: complete + coverageNoFiles},
		{
			name: "empty package alongside passed package",
			input: complete + `{"Action":"start","Package":"empty"}` + "\n" +
				`{"Action":"pass","Package":"empty"}` + "\n",
			want: "no tests ran in package: empty",
		},
		{name: "repeated test", input: start + run + pass + run + pass + done},
		{name: "empty stream", want: "no tests ran"},
		{name: "only generated", input: noFiles, want: "no tests ran"},
		{name: "no matching tests", input: start + done, want: "no tests ran"},
		{name: "test skipped", input: start + run + `{"Action":"skip","Package":"fixture","Test":"TestRequired"}` + "\n" + done, want: "test skip"},
		{name: "test failed", input: start + run + `{"Action":"fail","Package":"fixture","Test":"TestRequired"}` + "\n" + done, want: "test fail"},
		{name: "package failed", input: start + run + pass + `{"Action":"fail","Package":"fixture"}`, want: "package failed"},
		{name: "package skipped", input: start + `{"Action":"skip","Package":"fixture"}`, want: "package skipped"},
		{name: "package unfinished", input: start + run + pass, want: "package result missing"},
		{name: "test unfinished", input: start + run + done, want: "test result missing"},
		{name: "missing start", input: run + pass + done, want: "event outside package run"},
		{name: "duplicate start", input: start + start + run + pass + done, want: "unexpected package start"},
		{name: "duplicate run", input: start + run + run + pass + done, want: "test started twice"},
		{name: "missing run", input: start + pass + done, want: "test result without run"},
		{name: "after package end", input: complete + run, want: "event outside package run"},
		{name: "inactive continuation", input: start + `{"Action":"cont","Package":"fixture","Test":"TestRequired"}` + "\n" + done, want: "inactive test"},
		{name: "unknown action", input: start + `{"Action":"unknown","Package":"fixture","Test":"TestRequired"}` + "\n" + done, want: "unknown test action"},
		{name: "empty event", input: `{}`, want: "test event has no package"},
		{name: "wrong field type", input: `{"Action":42}`, want: "invalid test results"},
		{name: "malformed JSON", input: complete + "unexpected output", want: "invalid test results"},
		{name: "truncated JSON", input: complete + `{"Action":`, want: "invalid test results"},
		{name: "build failed", input: `{"Action":"build-fail","ImportPath":"fixture"}`, want: "build failed"},
		{name: "build path missing", input: `{"Action":"build-output"}`, want: "build event has no import path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkResults(strings.NewReader(tc.input), io.Discard)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSubtestSkipCannotHideBehindParentPass(t *testing.T) {
	input := `
{"Action":"start","Package":"fixture"}
{"Action":"run","Package":"fixture","Test":"TestRequired"}
{"Action":"run","Package":"fixture","Test":"TestRequired/child"}
{"Action":"skip","Package":"fixture","Test":"TestRequired/child"}
{"Action":"pass","Package":"fixture","Test":"TestRequired"}
{"Action":"pass","Package":"fixture"}
`
	err := checkResults(strings.NewReader(input), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "test skip: fixture/TestRequired/child") {
		t.Fatalf("error = %v, want skipped child", err)
	}
}

func TestOutputPreserved(t *testing.T) {
	input := `
{"Action":"start","Package":"fixture"}
{"Action":"run","Package":"fixture","Test":"TestRequired"}
{"Action":"output","Package":"fixture","Test":"TestRequired","Output":"=== RUN TestRequired\n"}
{"Action":"pause","Package":"fixture","Test":"TestRequired"}
{"Action":"cont","Package":"fixture","Test":"TestRequired"}
{"Action":"pass","Package":"fixture","Test":"TestRequired"}
{"Action":"output","Package":"fixture","Output":"ok fixture\n"}
{"Action":"pass","Package":"fixture"}
`
	var output strings.Builder
	if err := checkResults(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "=== RUN TestRequired\nok fixture\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestOutputWriteFailure(t *testing.T) {
	err := checkResults(strings.NewReader(`{"Action":"start","Package":"fixture","Output":"hello"}`), failedWriter{})
	if err == nil || !strings.Contains(err.Error(), "write test output") {
		t.Fatalf("error = %v, want output failure", err)
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) {
	return 0, errors.New("closed output")
}
