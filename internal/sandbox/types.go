// Package sandbox runs a bounded JSON-to-JSON Python transform in a disposable
// local container. Both the standalone CLI and versioned job adapter use it.
package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"
)

// CleanupGrace includes the independent cleanup deadline plus the CLI pipe
// wait bound. Queue leases must leave room for this after caller cancellation.
const CleanupGrace = controlTimeout + time.Second

const (
	ImageTag       = "durable-jobs-sandbox:python-v1"
	MaxScriptBytes = 16 << 10
	MaxInputBytes  = 64 << 10
	MaxOutputBytes = 64 << 10
	MaxStderrBytes = 4 << 10
	ownerLabel     = "org.durable-jobs.run"
	runnerLabel    = "org.durable-jobs.sandbox"
)

type Request struct {
	Script string          `json:"script"`
	Input  json.RawMessage `json:"input"`
}

type Result struct {
	Status           string          `json:"status"`
	Output           json.RawMessage `json:"output,omitempty"`
	FailureCode      string          `json:"failure_code,omitempty"`
	CleanupConfirmed bool            `json:"cleanup_confirmed"`
	Container        string          `json:"container,omitempty"`
	ImageID          string          `json:"image_id,omitempty"`
	ScriptSHA256     string          `json:"script_sha256,omitempty"`
	ElapsedMS        int64           `json:"elapsed_ms"`
}

func validObject(value []byte, limit int) bool {
	trimmed := bytes.TrimSpace(value)
	return len(value) <= limit && len(trimmed) > 0 && trimmed[0] == '{' && utf8.Valid(value) && json.Valid(value)
}

func (r Request) Validate() error {
	if len(r.Script) == 0 || len(r.Script) > MaxScriptBytes || !utf8.ValidString(r.Script) || !validObject(r.Input, MaxInputBytes) {
		return errors.New("invalid_request")
	}
	return nil
}
