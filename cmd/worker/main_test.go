package main

import (
	"context"
	"encoding/json"
	"testing"

	"durable-jobs/internal/jobs"
	"durable-jobs/internal/worker"
)

func TestSelectProcessor(t *testing.T) {
	p, config, err := selectProcessor("builtin")
	if err != nil || config != worker.DefaultConfig() {
		t.Fatalf("builtin configuration: %v, %v", config, err)
	}
	outcome, err := p.Process(context.Background(), jobs.Job{Type: "migrate_campaign", Payload: json.RawMessage(`{"campaign_id":"phase9_test","category":"Pets","created_at":"08/31/2026"}`)})
	if err != nil || outcome.Status != jobs.Succeeded {
		t.Fatalf("builtin job: %+v, %v", outcome, err)
	}
	outcome, err = p.Process(context.Background(), jobs.Job{Type: "run_script"})
	if err != nil || outcome.FailureCode != jobs.UnsupportedJobType {
		t.Fatalf("builtin mode must explicitly reject script jobs: %+v, %v", outcome, err)
	}
	p, config, err = selectProcessor("versioned")
	if _, ok := p.(worker.VersionedProcessor); !ok || err != nil || config != worker.ScriptConfig() {
		t.Fatalf("existing versioned mode changed: %T, %v, %v", p, config, err)
	}
	for _, name := range []string{"", "typo", "BUILTIN"} {
		if _, _, err := selectProcessor(name); err == nil {
			t.Fatalf("unknown processor %q accepted", name)
		}
	}
}
