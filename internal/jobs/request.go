package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"durable-jobs/internal/scripts"
)

type SubmitRequest struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Kept so regression tests can explicitly reject the original placeholder.
var ErrNotImplemented = errors.New("submission validation is not implemented")

// ParseSubmission validates the envelope, not the future worker's campaign rules.
func ParseSubmission(body []byte) (SubmitRequest, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' || !utf8.Valid(body) {
		return SubmitRequest{}, errors.New("body must be a UTF-8 JSON object")
	}
	var request SubmitRequest
	// Unmarshal rejects trailing JSON documents as well as malformed JSON.
	if err := json.Unmarshal(body, &request); err != nil {
		return SubmitRequest{}, errors.New("body must contain one valid job-submission object")
	}
	if request.Type != "migrate_campaign" && request.Type != "run_script" {
		return SubmitRequest{}, errors.New("type must be migrate_campaign or run_script")
	}
	request.Payload = bytes.TrimSpace(request.Payload)
	if len(request.Payload) == 0 || request.Payload[0] != '{' {
		return SubmitRequest{}, errors.New("payload must be a JSON object")
	}
	if request.Type == "run_script" {
		if _, err := scripts.Parse(request.Payload); err != nil {
			return SubmitRequest{}, err
		}
	}
	return request, nil
}
