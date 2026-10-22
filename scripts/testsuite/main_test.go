//go:build tools

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoTestProcessResults(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	runner := buildRunner(t)
	cases := []struct {
		name   string
		source string
		other  string
		args   []string
		want   string
	}{
		{
			name:   "success with generated package",
			source: `func TestRequired(t *testing.T) { t.Run("child", func(t *testing.T) { t.Parallel() }) }`,
		},
		{
			name:   "coverage with generated package",
			source: `func TestRequired(t *testing.T) {}`,
			args:   []string{"-cover", "./..."},
		},
		{name: "skipped test", source: `func TestRequired(t *testing.T) { t.Skip("fixture") }`, want: "test skip"},
		{
			name:   "skipped subtest",
			source: `func TestRequired(t *testing.T) { t.Run("child", func(t *testing.T) { t.Skip("fixture") }) }`,
			want:   "test skip",
		},
		{name: "failed test", source: `func TestRequired(t *testing.T) { t.Fatal("fixture") }`, want: "test fail"},
		{name: "runtime error", source: `func TestRequired(t *testing.T) { var zero int; _ = 1 / zero }`, want: "test process"},
		{name: "build failure", source: `func TestRequired(t *testing.T) { undefinedFixture() }`, want: "build failed"},
		{
			name:   "missing selection",
			source: `func TestRequired(t *testing.T) {}`,
			args:   []string{"-run=^TestMissing$", "./..."},
			want:   "no tests ran",
		},
		{
			name:   "successful TestMain without running suite",
			source: `func TestMain(m *testing.M) {} ; func TestRequired(t *testing.T) {}`,
			want:   "no tests ran",
		},
		{
			name:   "empty package alongside passed package",
			source: `func TestRequired(t *testing.T) {}`,
			other:  `func TestMain(m *testing.M) {} ; func TestRequired(t *testing.T) {}`,
			want:   "no tests ran in package: fixture/bypass",
		},
		{
			name:   "empty coverage package alongside passed package",
			source: `func TestRequired(t *testing.T) {}`,
			other:  `func TestMain(m *testing.M) {} ; func TestRequired(t *testing.T) {}`,
			args:   []string{"-cover", "./..."},
			want:   "no tests ran in package: fixture/bypass",
		},
		{
			name:   "unmatched package alongside passed package",
			source: `func TestRequired(t *testing.T) {}`,
			other:  `func TestExcluded(t *testing.T) {}`,
			args:   []string{"-run=^TestRequired$", "./..."},
			want:   "no tests ran in package: fixture/bypass",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			args := append([]string{"-count=1"}, tc.args...)
			if len(tc.args) == 0 {
				args = append(args, "./...")
			}
			command := exec.CommandContext(ctx, runner, args...)
			command.Dir = writeTestModule(t, tc.source, tc.other)
			output, err := command.CombinedOutput()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("run: %v\n%s", err, output)
				}
				return
			}
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() <= 0 {
				t.Fatalf("exit = %v, want nonzero status\n%s", err, output)
			}
			if !bytes.Contains(output, []byte(tc.want)) {
				t.Fatalf("output = %s, want %q", output, tc.want)
			}
		})
	}
}

func buildRunner(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "testsuite")
	command := exec.CommandContext(t.Context(), "go", "build", "-tags=tools", "-o", path, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, output)
	}
	return path
}

func TestStartFailure(t *testing.T) {
	err := runTests(t.Context(), filepath.Join(t.TempDir(), "missing-go"), nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "start tests") {
		t.Fatalf("error = %v, want start failure", err)
	}
}

func TestCancelledProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := runTests(ctx, "go", nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("cancelled test process succeeded")
	}
}

func writeTestModule(t *testing.T, source, other string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":              "module fixture\n\ngo 1.26.0\n",
		"required_test.go":    "package fixture\n\nimport \"testing\"\n\n" + source + "\n",
		"generated/source.go": "package generated\n\nfunc Value() int { return 1 }\n",
	}
	if other != "" {
		files["bypass/required_test.go"] = "package bypass\n\nimport \"testing\"\n\n" + other + "\n"
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
