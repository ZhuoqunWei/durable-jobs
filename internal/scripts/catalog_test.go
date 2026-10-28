package scripts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogAndStrictPayload(t *testing.T) {
	p := Payload{CampaignName, CampaignVersion, CampaignSHA256, "sha256:" + strings.Repeat("a", 64), json.RawMessage(`{"synthetic":true}`)}
	body, _ := json.Marshal(p)
	got, err := Parse(body)
	if err != nil || got.SourceSHA256 != CampaignSHA256 {
		t.Fatal(got, err)
	}
	source, err := Resolve(got)
	if err != nil || !strings.Contains(source, "def transform(record)") {
		t.Fatal("catalog integrity", err)
	}
	for _, bad := range []string{
		`null`, `[]`, `{}`, string(body) + `{}`, string(body[:len(body)-1]) + `,"script":"campaign_migration"}`,
		strings.Replace(string(body), `"script":`, `"Script":`, 1),
		strings.Replace(string(body), `"version":"1"`, `"version":"latest"`, 1),
		strings.Replace(string(body), `"version":"1"`, `"version":1`, 1),
		strings.Replace(string(body), CampaignSHA256, strings.Repeat("b", 64), 1),
		strings.Replace(string(body), p.ImageID, "durable-jobs-sandbox:python-v1", 1),
		strings.Replace(string(body), `{"synthetic":true}`, `null`, 1),
		strings.Replace(string(body), `{"synthetic":true}`, `[]`, 1),
		string(body[:len(body)-1]) + `,"source":"import os"}`,
		string(body[:len(body)-1]) + `,"path":"/tmp/script.py"}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid script payload: %s", bad)
		}
	}
	if _, err := Parse(append(body, 0xff)); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	if _, err := Parse([]byte(strings.Repeat(" ", 128<<10) + string(body))); err == nil {
		t.Fatal("oversize accepted")
	}
	p.Input = json.RawMessage(`{"x":"` + strings.Repeat("a", 64<<10) + `"}`)
	body, _ = json.Marshal(p)
	if _, err := Parse(body); err == nil {
		t.Fatal("oversized compact input accepted")
	}
}
