package jobs

import (
	"errors"
	"testing"
)

func TestParseSubmission(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"valid migration", `{"type":"migrate_campaign","payload":{"campaign_id":"campaign_42"}}`, false},
		{"missing type", `{"payload":{"campaign_id":"campaign_42"}}`, true},
		{"unsupported type", `{"type":"send_email","payload":{}}`, true},
		{"malformed JSON", `{"type":`, true},
		{"missing payload", `{"type":"migrate_campaign"}`, true},
		{"null payload", `{"type":"migrate_campaign","payload":null}`, true},
		{"array payload", `{"type":"migrate_campaign","payload":[]}`, true},
		{"trailing JSON", `{"type":"migrate_campaign","payload":{}} {}`, true},
		{"empty body", ``, true},
		{"top-level null", `null`, true},
		{"top-level array", `[]`, true},
		{"string payload", `{"type":"migrate_campaign","payload":"hello"}`, true},
		{"numeric type", `{"type":42,"payload":{}}`, true},
		{"empty object payload", `{"type":"migrate_campaign","payload":{}}`, false},
		{"unknown field ignored", `{"type":"migrate_campaign","payload":{},"extra":true}`, false},
		{"whitespace", " \n{\"type\":\"migrate_campaign\",\"payload\":{}}\t", false},
		{"invalid UTF-8", string([]byte{'{', '"', 0xff, '"', ':', '1', '}'}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, err := ParseSubmission([]byte(tt.body))
			if errors.Is(err, ErrNotImplemented) {
				t.Fatal("implement ParseSubmission; the placeholder is not validation")
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v; want error = %v", err, tt.wantErr)
			}
			if !tt.wantErr && (request.Type != "migrate_campaign" || len(request.Payload) == 0) {
				t.Fatalf("decoded request is incomplete: %+v", request)
			}
		})
	}
}
