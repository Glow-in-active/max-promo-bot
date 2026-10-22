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

var domainMutations = []mutation{
	{ID: "campaign-start", File: "internal/domain/model.go", Function: "CanAct", Original: "now.Before(c.StartsAt)", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "campaign-end", File: "internal/domain/model.go", Function: "CanAct", Original: "!now.Before(c.EndsAt)", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "daily-chance-unique", File: "internal/domain/model.go", Function: "GrantChance", Original: "a.DayID == day.ID", Changed: "a.DayID != day.ID", Package: "./internal/domain", Tests: "."},
	{ID: "chance-limit", File: "internal/domain/model.go", Function: "GrantChance", Original: "len(active) >= 10", Changed: "len(active) > 10", Package: "./internal/domain", Tests: "."},
	{ID: "day-lower-bound", File: "internal/domain/model.go", Function: "GrantChance", Original: "day.Number < 1", Changed: "day.Number < 0", Package: "./internal/domain", Tests: "."},
	{ID: "registration-deadline", File: "internal/domain/model.go", Function: "CanRegister", Original: "!now.Before(d.ClosesAt)", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "draw-state", File: "internal/domain/model.go", Function: "CanDraw", Original: "d.State != DrawOpen", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "approval-state", File: "internal/domain/model.go", Function: "CanApprove", Original: "d.State != DrawDrawn", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "prize-owner", File: "internal/domain/model.go", Function: "CanClaim", Original: "o.UserID != userID", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "prize-consent", File: "internal/domain/model.go", Function: "CanClaim", Original: "!consent", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "prize-deadline", File: "internal/domain/model.go", Function: "CanExpire", Original: "!now.Before(*o.Deadline)", Changed: "true", Package: "./internal/domain", Tests: "."},
	{ID: "candidate-unique", File: "internal/domain/random.go", Function: "Select", Original: "seen[c.UserID]", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "positive-weight", File: "internal/domain/random.go", Function: "Select", Original: "c.Weight < 1", Changed: "c.Weight < 0", Package: "./internal/domain", Tests: "."},
	{ID: "random-upper-bound", File: "internal/domain/random.go", Function: "Select", Original: "pick >= total", Changed: "pick > total", Package: "./internal/domain", Tests: "."},
	{ID: "weighted-boundary", File: "internal/domain/random.go", Function: "Select", Original: "pick < int64(c.Weight)", Changed: "pick <= int64(c.Weight)", Package: "./internal/domain", Tests: "."},
	{ID: "final-reserve-required", File: "internal/domain/setup_validation.go", Function: "validateSetupDraws", Original: "final.ReserveCount < 1", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "explicit-timezone", File: "internal/domain/setup_validation.go", Function: "validateCampaignSetup", Original: "c.Timezone == \"\"", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "host-timezone", File: "internal/domain/setup_validation.go", Function: "validateCampaignSetup", Original: "c.Timezone == \"Local\"", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "calendar-date-unique", File: "internal/domain/setup_validation.go", Function: "validateDayCalendar", Original: "dayDates[date]", Changed: "false", Package: "./internal/domain", Tests: "."},
	{ID: "calendar-midnight-boundary", File: "internal/domain/setup_validation.go", Function: "validateDayCalendar", Original: "d.EndsAt.After(nextDate)", Changed: "d.EndsAt.After(nextDate) && false", Package: "./internal/domain", Tests: "."},
}

var integrationMutations = []mutation{
	{ID: "callback-source-day", File: "internal/app/execution.go", Function: "loadPartnerDay", Original: "tx.DayAt(ctx, campaign.ID, cmd.OccurredAt)", Changed: "tx.DayAt(ctx, campaign.ID, execution.now)", Package: "./internal/app", Tests: "^TestReplayedCallbackRemainsBoundToItsOriginalDay$"},
	{ID: "subscription-source-day", File: "internal/app/execution.go", Function: "checkSubscription", Original: "s.repo.CurrentDay(ctx, cmd.CampaignID, cmd.OccurredAt)", Changed: "s.repo.CurrentDay(ctx, cmd.CampaignID, s.Now())", Package: "./internal/app", Tests: "^TestQueuedSubscriptionCheckCannotSwitchPartnersAfterMidnight$"},
	{ID: "provider-retry-after", File: "internal/app/outbox.go", Function: "recordDeliveryFailure", Original: "max(delay, provider.RetryDelay())", Changed: "max(delay, 0)", Package: "./internal/app", Tests: "^TestApplicationProviderRetryAfterControlsDurableOutbox$"},
	{ID: "partner-stock-grouping", File: "internal/storage/postgres/stats.go", Function: "partnerPromoStock", Original: "!exists", Changed: "!exists || exists", Package: "./internal/transport/httpapi", Tests: "^TestStatsAPIReportsRepeatedPartnerNameAndTotal$"},
	{ID: "membership-required-e2e", File: "internal/app/participation.go", Function: "registerParticipation", Original: "execution.subscription != SubscriptionYes", Changed: "false", Package: "./internal/app", Tests: "^TestMAXEndToEndCampaignLifecycle$"},
	{ID: "callback-replay-identity", File: "internal/transport/httpapi/max_update.go", Function: "callbackCommand", Original: `fmt.Sprintf("callback:%d:%d:%q:%q", u.Timestamp, u.Callback.User.UserID, u.Callback.CallbackID, u.Callback.Payload)`, Changed: `fmt.Sprintf("callback:%d:%d:%q:%q", time.Now().UnixNano(), u.Callback.User.UserID, u.Callback.CallbackID, u.Callback.Payload)`, Package: "./internal/app", Tests: "^TestReplayedClaimCallbackCannotReplaceAnotherPrizeConversation$"},
	{ID: "recipient-send-limit", File: "internal/platform/maxapi/client.go", Function: "Send", Original: "&recipientRequest{c.sends, message.UserID}", Changed: "&recipientRequest{c.sends, 0}", Package: "./internal/platform/maxapi", Tests: "^TestSendingToAnotherDialogDoesNotWaitForThePreviousDialog$"},
	{ID: "max-total-request-limit", File: "internal/platform/maxapi/client.go", Function: "New", Original: "newPacer(time.Second/30 + time.Nanosecond)", Changed: "newPacer(0)", Package: "./internal/platform/maxapi", Tests: "^TestAllMAXMethodsShareTheRequestLimit$"},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("mutate", flag.ContinueOnError)
	integration := flags.Bool("integration", false, "Run cross-layer regressions against TEST_DATABASE_URL")
	list := flags.Bool("list", false, "List selected mutations")
	validate := flags.Bool("validate", false, "Validate mutation selectors without executing tests")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	mutations, reportName := domainMutations, "mutations.json"
	if *integration {
		mutations, reportName = integrationMutations, "integration-mutations.json"
	}
	if *list {
		return json.NewEncoder(os.Stdout).Encode(mutations)
	}
	if *validate {
		for _, m := range mutations {
			if _, err := replaceExpression(filepath.Join(root, m.File), m); err != nil {
				return err
			}
		}
		fmt.Printf("All %d mutation selectors match exactly once.\n", len(mutations))
		return nil
	}
	if *integration && os.Getenv("TEST_DATABASE_URL") == "" {
		return errors.New("integration mutations require TEST_DATABASE_URL pointing to a disposable database")
	}
	fmt.Println("Checking the unmodified test suites...")
	baselines := map[string]bool{}
	for _, m := range mutations {
		key := m.Package + " " + m.Tests
		if baselines[key] {
			continue
		}
		output, testErr, timedOut := runTests(m, "", *integration)
		if status := testOutcome(output, testErr, timedOut, m.Tests); status != "survived" {
			return fmt.Errorf("baseline %s failed (%s); no mutation score was computed:\n%s", key, status, output)
		}
		baselines[key] = true
		fmt.Printf("baseline PASS %s\n", key)
	}
	temp, err := os.MkdirTemp("", "promo-mutation-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	results := make([]outcome, 0, len(mutations))
	failed := false
	for _, m := range mutations {
		original := filepath.Join(root, m.File)
		changed, err := replaceExpression(original, m)
		if err != nil {
			return err
		}
		mutatedPath := filepath.Join(temp, m.ID+".go")
		if err := os.WriteFile(mutatedPath, changed, 0600); err != nil {
			return err
		}
		overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{original: mutatedPath}})
		if err != nil {
			return err
		}
		overlayPath := filepath.Join(temp, "overlay.json")
		if err := os.WriteFile(overlayPath, overlay, 0600); err != nil {
			return err
		}
		output, testErr, timedOut := runTests(m, overlayPath, *integration)
		result := outcome{mutation: m, Status: testOutcome(output, testErr, timedOut, m.Tests)}
		if result.Status != "killed" {
			failed = true
			result.Detail = string(output)
		}
		fmt.Printf("%-8s %s\n", result.Status, result.ID)
		results = append(results, result)
	}
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
	if failed {
		return fmt.Errorf("mutation gate failed; inspect %s", reportPath)
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
