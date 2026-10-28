package campaign

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMigrateCampaign(t *testing.T) {
	for _, tc := range []struct{ category, target, date, wantDate string }{
		{"Pets", "animals", "08/31/2026", "2026-08-31"},
		{"Medical", "medical", "2024-02-29", "2024-02-29"},
		{"Education", "education", "02/29/2024", "2024-02-29"},
		{"Pets", "animals", "0001-01-01", "0001-01-01"},
	} {
		input, _ := json.Marshal(LegacyCampaign{ID: "synthetic_42", Category: tc.category, CreatedAt: tc.date})
		got, err := Migrate(input)
		want := MigratedCampaign{ID: "synthetic_42", CategoryID: tc.target, CreatedDate: tc.wantDate, SchemaVersion: 1}
		if err != nil || got != want {
			t.Fatalf("Migrate(%s) = %+v, %v; want %+v", input, got, err, want)
		}
		again, err := Migrate(input)
		if err != nil || again != got {
			t.Fatal("migration is not deterministic")
		}
	}
}

func TestMigrateRejectsInvalidCampaigns(t *testing.T) {
	valid := `{"campaign_id":"synthetic_42","category":"Pets","created_at":"08/31/2026"}`
	inputs := []string{"", "null", "[]", "{}", `{`, valid + `{}`,
		strings.Replace(valid, "synthetic_42", "", 1),
		strings.Replace(valid, "synthetic_42", "with spaces", 1),
		strings.Replace(valid, "synthetic_42", strings.Repeat("x", 129), 1),
		strings.Replace(valid, `"synthetic_42"`, "42", 1),
		strings.Replace(valid, "Pets", "Unknown", 1),
		strings.Replace(valid, "Pets", "pets", 1),
		strings.Replace(valid, "category", "Category", 1),
		strings.Replace(valid, "campaign_id", "CAMPAIGN_ID", 1),
		strings.Replace(valid, "08/31/2026", "02/29/2026", 1),
		strings.Replace(valid, "08/31/2026", "2026-02-29", 1),
		strings.Replace(valid, "08/31/2026", "8/31/2026", 1),
		strings.Replace(valid, "08/31/2026", "0000-01-01", 1),
		strings.Replace(valid, "08/31/2026", "2026-08-31T00:00:00Z", 1),
		strings.Replace(valid, `}`, `,"unexpected":true}`, 1),
		strings.Replace(valid, "Pets", string([]byte{0xff}), 1),
		strings.Repeat(" ", 64<<10) + valid,
	}
	for i, input := range inputs {
		if _, err := Migrate([]byte(input)); !errors.Is(err, ErrInvalidCampaign) {
			t.Errorf("case %d: error=%v", i, err)
		}
	}
}
