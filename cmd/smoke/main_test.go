package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"durable-jobs/internal/jobs"
)

func TestVerify(t *testing.T) {
	for _, scenario := range []string{"pass", "not-ready", "missing-id", "different-id", "failed", "wrong-result", "multiple-attempts", "missing-time", "bad-replay", "missing-conflict", "bad-history", "never-completes", "invalid-json", "unknown-status", "redirect", "stale-fields"} {
		t.Run(scenario, func(t *testing.T) {
			var submissions atomic.Int64
			var polls atomic.Int64
			now := time.Now().UTC()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				job := jobs.Job{ID: "job-test", Status: jobs.Succeeded, Attempts: 1, StartedAt: &now, CompletedAt: &now,
					Result: json.RawMessage(`{"campaign_id":"phase9_smoke","category_id":"animals","created_date":"2026-08-31","schema_version":1}`)}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/readyz":
					if scenario == "not-ready" {
						w.WriteHeader(http.StatusServiceUnavailable)
					} else if scenario == "redirect" {
						http.Redirect(w, r, "/elsewhere", http.StatusFound)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/jobs":
					if r.Header.Get("Idempotency-Key") != submissionKey || r.Header.Get("Content-Type") != "application/json" {
						t.Error("smoke omitted submission headers")
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					if strings.Contains(string(body), "Medical") {
						if scenario != "missing-conflict" {
							w.WriteHeader(http.StatusConflict)
						}
						return
					}
					if string(body) != submission {
						t.Error("unexpected synthetic submission")
					}
					n := submissions.Add(1)
					if scenario == "missing-id" {
						job.ID = ""
					} else if scenario == "bad-replay" && n > 1 {
						job.ID = "another-job"
					}
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(job)
				case r.URL.Path == "/jobs/job-test":
					n := polls.Add(1)
					switch scenario {
					case "different-id":
						job.ID = "another-job"
					case "failed":
						job.Status = jobs.Failed
					case "wrong-result":
						job.Result = json.RawMessage(`{}`)
					case "multiple-attempts":
						job.Attempts = 2
					case "missing-time":
						job.CompletedAt = nil
					case "never-completes":
						job.Status = jobs.Queued
					case "unknown-status":
						job.Status = "pretend-success"
					case "stale-fields":
						if n == 1 {
							job.Status = jobs.Running
						} else {
							job.Result = nil
							job.StartedAt, job.CompletedAt = nil, nil
						}
					case "invalid-json":
						_, _ = io.WriteString(w, "{")
						return
					}
					_ = json.NewEncoder(w).Encode(job)
				case r.URL.Path == "/jobs/job-test/attempts":
					history := []jobs.Attempt{{Number: 1, Status: "succeeded", FinishedAt: &now}}
					if scenario == "bad-history" {
						history = nil
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"job_id": job.ID, "attempts": history})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			duration := time.Second
			if scenario == "never-completes" {
				duration = 25 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), duration)
			defer cancel()
			got, err := verify(ctx, server.URL, false)
			if scenario == "pass" {
				if err != nil || got.Status != "passed" || got.Checks != 6 || got.JobID != "job-test" {
					t.Fatalf("verification = %+v, %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("false success for %s: %+v", scenario, got)
			}
		})
	}
}

func TestReadinessNeverSubmits(t *testing.T) {
	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/readyz" {
			t.Errorf("health check must be read-only, got %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	got, err := verify(context.Background(), server.URL, true)
	if err != nil || got.Status != "ready" || count.Load() != 1 {
		t.Fatalf("readiness = %+v, %v, requests %d", got, err, count.Load())
	}
}

func TestRejectInvalidURL(t *testing.T) {
	for _, address := range []string{"", "ftp://localhost", "http://user:password@localhost", "http://localhost/elsewhere", "http://localhost?q=1", "http://localhost#fragment"} {
		if _, err := verify(context.Background(), address, true); err == nil {
			t.Errorf("accepted %q", address)
		}
	}
}
