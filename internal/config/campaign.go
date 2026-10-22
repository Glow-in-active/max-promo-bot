package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"promo/internal/domain"
)

const maxCampaignBytes = 2 << 20

func LoadCampaign(path string) (domain.Setup, error) {
	file, err := os.Open(path)
	if err != nil {
		return domain.Setup{}, fmt.Errorf("open campaign: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCampaignBytes+1))
	if err != nil {
		return domain.Setup{}, fmt.Errorf("read campaign: %w", err)
	}
	if len(data) > maxCampaignBytes {
		return domain.Setup{}, validationError("campaign file exceeds 2 MiB")
	}
	var setup domain.Setup
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&setup); err != nil {
		switch {
		case err == io.EOF:
			return domain.Setup{}, validationError("campaign file is empty")
		case err == io.ErrUnexpectedEOF:
			return domain.Setup{}, validationError("campaign file contains incomplete JSON")
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return domain.Setup{}, validationError("campaign file contains unknown fields")
		}
		return domain.Setup{}, fmt.Errorf("decode campaign: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return domain.Setup{}, validationError("campaign file must contain exactly one JSON document")
	}
	return setup.Prepare()
}
