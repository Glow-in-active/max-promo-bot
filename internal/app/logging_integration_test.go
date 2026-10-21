//go:build integration

package app_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"promo/internal/app"
	"promo/internal/observability"
)

func TestDrawLogsDescribeCommittedChangesOnly(t *testing.T) {
	h := setup(t, "final")
	ctx := context.Background()
	var output bytes.Buffer
	logger, err := observability.New(&output, "info")
	must(t, err)
	fault := errors.New("injected private enqueue failure")
	repo := &enqueueFailpointRepository{Repository: h.store, failAt: 2, err: fault}
	service := app.New(repo, h.messenger, h.clock, firstCandidate{}, app.WithLogger(logger))
	_, err = service.RunDraw(ctx, h.run.CampaignID, h.run.FinalDrawID, "admin")
	must(t, err)
	_, err = service.RunDraw(ctx, h.run.CampaignID, h.run.FinalDrawID, "admin")
	must(t, err)
	_, err = service.ApproveDraw(ctx, h.run.CampaignID, h.run.FinalDrawID, "admin")
	if !errors.Is(err, fault) {
		t.Fatalf("did not exercise failed approval: %v", err)
	}
	if strings.Contains(output.String(), "draw approved") {
		t.Fatalf("rolled-back approval was logged as completed: %s", output.String())
	}
	_, err = service.ApproveDraw(ctx, h.run.CampaignID, h.run.FinalDrawID, "admin")
	must(t, err)
	_, err = service.ApproveDraw(ctx, h.run.CampaignID, h.run.FinalDrawID, "admin")
	must(t, err)
	if strings.Count(output.String(), `"msg":"draw completed"`) != 1 || strings.Count(output.String(), `"msg":"draw approved"`) != 1 {
		t.Fatalf("replays duplicated lifecycle logs: %s", output.String())
	}
}
