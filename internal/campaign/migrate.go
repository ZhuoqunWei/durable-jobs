// Package campaign owns the synthetic migration's business rules. It has no
// knowledge of HTTP, PostgreSQL, workers, Docker or language models.
package campaign

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"
	"unicode/utf8"
)

var ErrInvalidCampaign = errors.New("campaign does not satisfy the synthetic migration contract")

var campaignIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type LegacyCampaign struct {
	ID        string `json:"campaign_id"`
	Category  string `json:"category"`
	CreatedAt string `json:"created_at"`
}

type MigratedCampaign struct {
	ID            string `json:"campaign_id"`
	CategoryID    string `json:"category_id"`
	CreatedDate   string `json:"created_date"`
	SchemaVersion int    `json:"schema_version"`
}

// Migrate is deterministic: identical input yields identical output. The tiny
// taxonomy below is invented for this demo, not taken from any employer.
// Unknown fields/categories fail instead of silently discarding or guessing data.
func Migrate(payload []byte) (MigratedCampaign, error) {
	if len(payload) > 64<<10 || !utf8.Valid(payload) {
		return MigratedCampaign{}, ErrInvalidCampaign
	}
	// Check exact field names before decoding the struct: encoding/json's
	// struct matching is otherwise case-insensitive. Duplicate-key behavior
	// remains the same as the submission/JSONB layer (last value wins).
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || len(fields) != 3 {
		return MigratedCampaign{}, ErrInvalidCampaign
	}
	for _, name := range []string{"campaign_id", "category", "created_at"} {
		if _, exists := fields[name]; !exists {
			return MigratedCampaign{}, ErrInvalidCampaign
		}
	}
	var legacy LegacyCampaign
	if err := json.Unmarshal(payload, &legacy); err != nil {
		return MigratedCampaign{}, ErrInvalidCampaign
	}
	if !campaignIDPattern.MatchString(legacy.ID) {
		return MigratedCampaign{}, ErrInvalidCampaign
	}
	var category string
	switch legacy.Category {
	case "Pets":
		category = "animals"
	case "Medical":
		category = "medical"
	case "Education":
		category = "education"
	default:
		return MigratedCampaign{}, ErrInvalidCampaign
	}
	var date time.Time
	validDate := false
	for _, layout := range []string{"01/02/2006", time.DateOnly} {
		parsed, err := time.Parse(layout, legacy.CreatedAt)
		if err == nil && parsed.Year() >= 1 && parsed.Format(layout) == legacy.CreatedAt {
			date = parsed
			validDate = true
			break
		}
	}
	if !validDate {
		return MigratedCampaign{}, ErrInvalidCampaign
	}
	return MigratedCampaign{ID: legacy.ID, CategoryID: category, CreatedDate: date.Format(time.DateOnly), SchemaVersion: 1}, nil
}
