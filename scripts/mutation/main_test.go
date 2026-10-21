//go:build tools

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMutationRequiresFailureOfSelectedTest(t *testing.T) {
	failed := errors.New("go test exited with status 1")
	tests := []struct {
		name    string
		output  string
		err     error
		timeout bool
		want    string
	}{
		{"assertion", `{"Action":"run","Test":"TestInvariant"}
{"Action":"fail","Test":"TestInvariant"}`, failed, false, "killed"},
		{"survivor", `{"Action":"run","Test":"TestInvariant"}
{"Action":"pass","Test":"TestInvariant"}`, nil, false, "survived"},
		{"unrelated failure", `{"Action":"run","Test":"TestInvariant"}
{"Action":"pass","Test":"TestInvariant"}
{"Action":"fail","Test":"TestOther"}`, failed, false, "invalid"},
		{"build failure", `{"Action":"build-fail","Package":"promo/internal/app"}`, failed, false, "invalid"},
		{"missing test", `{"Action":"pass","Package":"promo/internal/app"}`, nil, false, "invalid"},
		{"skipped test", `{"Action":"run","Test":"TestInvariant"}
{"Action":"skip","Test":"TestInvariant"}`, nil, false, "invalid"},
		{"incomplete stream", `{"Action":"run","Test":"TestInvariant"}`, nil, false, "invalid"},
		{"malformed stream", `{"Action":"run","Test":"TestInvariant"}
not test output`, failed, false, "invalid"},
		{"timeout after failure", `{"Action":"run","Test":"TestInvariant"}
{"Action":"fail","Test":"TestInvariant"}`, failed, true, "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := testOutcome([]byte(tt.output), tt.err, tt.timeout, "^TestInvariant$")
			if got != tt.want {
				t.Fatalf("outcome=%q, want=%q", got, tt.want)
			}
		})
	}
}

func TestMutationSelectorChangesOneReadAndLeavesSourceUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.go")
	source := "package sample\nfunc selectCode(seen map[int]bool, user int) bool {\nif seen[user] { return false }; seen[user] = true; return true\n}\n"
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	m := mutation{ID: "unique-user", Function: "selectCode", Original: "seen[user]", Changed: "false"}
	changed, err := replaceExpression(path, m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(changed), "if false {") || !strings.Contains(string(changed), "seen[user] = true") {
		t.Fatalf("mutation modified the wrong expression:\n%s", changed)
	}
	original, err := os.ReadFile(path)
	if err != nil || string(original) != source {
		t.Fatalf("source changed: err=%v", err)
	}

	m.Original = "unknown(user)"
	if _, err = replaceExpression(path, m); err == nil {
		t.Fatal("a missing mutation selector was accepted")
	}
	m.Original = "true"
	if _, err = replaceExpression(path, m); err == nil {
		t.Fatal("an ambiguous mutation selector was accepted")
	}
}
