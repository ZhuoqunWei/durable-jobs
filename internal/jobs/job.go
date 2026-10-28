package jobs

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound       = errors.New("job not found")
	ErrInvalidPayload = errors.New("payload cannot be represented as PostgreSQL JSONB")
)

type Job struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	Status         State           `json:"status"`
	CreatedAt      time.Time       `json:"created_at"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	AvailableAt    time.Time       `json:"available_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	FailureCode    string          `json:"failure_code,omitempty"`
}
