package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"promo/internal/domain"
	"promo/internal/observability"
)

func TestLoadCampaignPreparesProductionExample(t *testing.T) {
	setup, err := LoadCampaign("../../config/campaign.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if setup.Campaign.ID != "autumn-promo-2026" || len(setup.Days) != 10 || len(setup.Draws) != 11 || len(setup.Promos) != 20 {
		t.Fatal("campaign example was not loaded completely")
	}
	for _, draw := range setup.Draws {
		if draw.ClaimTTL != 24*time.Hour {
			t.Fatal("claim period was not prepared from seconds")
		}
	}
}

func TestLoadCampaignAcceptsWhitespaceAndExactSizeLimit(t *testing.T) {
	data := campaignExample(t)
	for name, suffix := range map[string][]byte{
		"whitespace": []byte(" \n\r\t"),
		"size limit": bytes.Repeat([]byte(" "), (2<<20)-len(data)),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeCampaign(t, append(bytes.Clone(data), suffix...))
			if _, err := LoadCampaign(path); err != nil {
				t.Fatalf("valid campaign rejected: %v", err)
			}
		})
	}
}

func TestLoadCampaignRejectsAdditionalDocumentsAndUnknownFields(t *testing.T) {
	data := campaignExample(t)
	for name, input := range map[string][]byte{
		"second object": append(bytes.Clone(data), []byte("\n{}")...),
		"second scalar": append(bytes.Clone(data), []byte(" true")...),
		"garbage":       append(bytes.Clone(data), []byte("\ngarbage")...),
		"unknown root":  bytes.Replace(data, []byte(`"claim_ttl_seconds": 86400`), []byte(`"unknown": true, "claim_ttl_seconds": 86400`), 1),
		"unknown field": bytes.Replace(data, []byte(`"name": "Десять дней подарков"`), []byte(`"unknown": true, "name": "Десять дней подарков"`), 1),
		"empty":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCampaign(writeCampaign(t, input)); err == nil {
				t.Fatal("invalid campaign JSON accepted")
			}
		})
	}
}

func TestLoadCampaignRejectsOversizedFilesBeforeDecoding(t *testing.T) {
	data := campaignExample(t)
	for name, input := range map[string][]byte{
		"invalid JSON":               bytes.Repeat([]byte("x"), (2<<20)+1),
		"valid JSON with whitespace": append(bytes.Clone(data), bytes.Repeat([]byte(" "), (2<<20)+1-len(data))...),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadCampaign(writeCampaign(t, input))
			if err == nil || !strings.Contains(err.Error(), "exceeds 2 MiB") {
				t.Fatalf("oversized file was not rejected by its size: %v", err)
			}
		})
	}
}

func TestLoadCampaignRejectsInvalidDomainConfiguration(t *testing.T) {
	var setup domain.Setup
	if err := json.Unmarshal(campaignExample(t), &setup); err != nil {
		t.Fatal(err)
	}
	setup.Days = setup.Days[:9]
	data, err := json.Marshal(setup)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadCampaign(writeCampaign(t, data))
	fault, ok := domain.FaultOf(err)
	if !ok || fault.Code != domain.Invalid {
		t.Fatalf("invalid campaign did not reach domain validation: %v", err)
	}
}

func TestLoadCampaignReportsFileErrors(t *testing.T) {
	_, err := LoadCampaign(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error lost: %v", err)
	}
	if _, err := LoadCampaign(t.TempDir()); err == nil {
		t.Fatal("directory accepted as a campaign file")
	}
}

func TestLoadCampaignErrorsHaveSafeDiagnostics(t *testing.T) {
	data := campaignExample(t)
	for name, input := range map[string][]byte{
		"oversized": bytes.Repeat([]byte(" "), (2<<20)+1),
		"trailing":  append(bytes.Clone(data), []byte(" secret-token Иван")...),
		"unknown":   bytes.Replace(data, []byte(`"claim_ttl_seconds": 86400`), []byte(`"secret-token-Иван": true, "claim_ttl_seconds": 86400`), 1),
		"empty":     nil,
		"truncated": []byte(`{"claim_ttl_seconds":`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadCampaign(writeCampaign(t, input))
			if err == nil {
				t.Fatal("invalid campaign accepted")
			}
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			logger.LogAttrs(t.Context(), slog.LevelError, "campaign load failed", observability.ErrorAttrs(err)...)
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["error_code"] != "invalid_configuration" || record["reason"] == nil {
				t.Fatalf("campaign failure has no safe reason: %v", record)
			}
			if strings.Contains(output.String(), "secret-token") || strings.Contains(output.String(), "Иван") {
				t.Fatalf("campaign data reached diagnostics: %s", output.String())
			}
		})
	}
}

func campaignExample(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../config/campaign.example.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeCampaign(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "campaign.json")
	if err := os.WriteFile(path, data, 0400); err != nil {
		t.Fatal(err)
	}
	return path
}
