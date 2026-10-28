// Command smoke verifies the actual API/worker path using synthetic data only.
// It runs inside the same static, non-root image as the service.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"durable-jobs/internal/campaign"
	"durable-jobs/internal/jobs"
)

const submission = `{"type":"migrate_campaign","payload":{"campaign_id":"phase9_smoke","category":"Pets","created_at":"08/31/2026"}}`
const submissionKey = "phase9-smoke-v1"

type report struct {
	Status   string `json:"status"`
	JobID    string `json:"job_id"`
	Attempts int    `json:"attempts"`
	Checks   int    `json:"checks"`
}

func main() {
	address := flag.String("url", "http://127.0.0.1:8085", "API base URL; use an isolated synthetic database")
	timeout := flag.Duration("timeout", 30*time.Second, "total verification deadline")
	readyOnly := flag.Bool("ready-only", false, "only check readiness; never submit a job")
	flag.Parse()
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "timeout must be positive")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := verify(ctx, *address, *readyOnly)
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke failed:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func verify(ctx context.Context, address string, readyOnly bool) (report, error) {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return report{}, errors.New("url must be an HTTP(S) origin without credentials, query or path")
	}
	base := strings.TrimRight(address, "/")
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	if err := request(ctx, client, http.MethodGet, base+"/readyz", nil, "", http.StatusOK, nil); err != nil {
		return report{}, fmt.Errorf("readiness: %w", err)
	}
	if readyOnly {
		return report{Status: "ready", Checks: 1}, nil
	}
	var created jobs.Job
	if err := request(ctx, client, http.MethodPost, base+"/jobs", []byte(submission), submissionKey, http.StatusAccepted, &created); err != nil {
		return report{}, fmt.Errorf("submit: %w", err)
	}
	if created.ID == "" {
		return report{}, errors.New("submission returned no job ID")
	}
	var completed jobs.Job
	for {
		// Decode each poll into a fresh value: omitted fields must not inherit
		// output or timestamps from an earlier response and create false proof.
		var observed jobs.Job
		if err := request(ctx, client, http.MethodGet, base+"/jobs/"+url.PathEscape(created.ID), nil, "", http.StatusOK, &observed); err != nil {
			return report{}, fmt.Errorf("retrieve: %w", err)
		}
		completed = observed
		if completed.ID != created.ID {
			return report{}, errors.New("retrieval returned another job")
		}
		if completed.Status == jobs.Succeeded {
			break
		}
		if completed.Status == jobs.Failed {
			return report{}, fmt.Errorf("job failed: %s", completed.FailureCode)
		}
		if completed.Status != jobs.Queued && completed.Status != jobs.Running && completed.Status != jobs.RetryWaiting {
			return report{}, fmt.Errorf("unknown job status: %s", completed.Status)
		}
		select {
		case <-ctx.Done():
			return report{}, fmt.Errorf("job did not complete: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	want := campaign.MigratedCampaign{ID: "phase9_smoke", CategoryID: "animals", CreatedDate: "2026-08-31", SchemaVersion: 1}
	var got campaign.MigratedCampaign
	if err := json.Unmarshal(completed.Result, &got); err != nil || got != want || completed.Attempts != 1 || completed.CompletedAt == nil || completed.StartedAt == nil || completed.FailureCode != "" {
		return report{}, errors.New("completed job has wrong result, timestamps or attempt count")
	}
	var replay jobs.Job
	if err := request(ctx, client, http.MethodPost, base+"/jobs", []byte(submission), submissionKey, http.StatusAccepted, &replay); err != nil {
		return report{}, fmt.Errorf("replay: %w", err)
	}
	if replay.ID != completed.ID || replay.Status != jobs.Succeeded || replay.Attempts != 1 || !bytes.Equal(replay.Result, completed.Result) {
		return report{}, errors.New("keyed replay did not return the completed job")
	}
	changed := strings.Replace(submission, "Pets", "Medical", 1)
	if err := request(ctx, client, http.MethodPost, base+"/jobs", []byte(changed), submissionKey, http.StatusConflict, nil); err != nil {
		return report{}, fmt.Errorf("changed-input conflict: %w", err)
	}
	var history struct {
		JobID    string         `json:"job_id"`
		Attempts []jobs.Attempt `json:"attempts"`
	}
	if err := request(ctx, client, http.MethodGet, base+"/jobs/"+url.PathEscape(created.ID)+"/attempts", nil, "", http.StatusOK, &history); err != nil {
		return report{}, fmt.Errorf("history: %w", err)
	}
	if history.JobID != created.ID || len(history.Attempts) != 1 || history.Attempts[0].Number != 1 || history.Attempts[0].Status != "succeeded" || history.Attempts[0].FinishedAt == nil {
		return report{}, errors.New("expected one committed successful attempt")
	}
	return report{Status: "passed", JobID: created.ID, Attempts: completed.Attempts, Checks: 6}, nil
}

func request(ctx context.Context, client *http.Client, method, address string, body []byte, key string, wantStatus int, target any) error {
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		return fmt.Errorf("HTTP %d, expected %d", response.StatusCode, wantStatus)
	}
	if target != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(target)
	}
	return nil
}
