package database

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"durable-jobs/internal/httpapi"
	"durable-jobs/internal/jobs"
	"durable-jobs/internal/worker"
)

//go:embed testdata/campaign-fixtures.json
var campaignFixtures []byte

type fixtureOutcome struct {
	Status      jobs.State      `json:"status"`
	Attempts    int             `json:"attempts"`
	FailureCode string          `json:"failure_code,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

func TestCampaignFixtureMatrixIntegration(t *testing.T) {
	var fixtures []struct {
		Name        string          `json:"name"`
		Payload     json.RawMessage `json:"payload"`
		Status      jobs.State      `json:"status"`
		FailureCode string          `json:"failure_code"`
		Result      json.RawMessage `json:"result"`
	}
	decoder := json.NewDecoder(bytes.NewReader(campaignFixtures))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixtures); err != nil || len(fixtures) != 6 {
		t.Fatalf("fixture contract: count=%d error=%v", len(fixtures), err)
	}
	seen := map[string]bool{}
	for _, fixture := range fixtures {
		if fixture.Name == "" || seen[fixture.Name] {
			t.Fatal("fixture names must be nonempty and unique")
		}
		seen[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			pool, _ := isolatedDatabase(t)
			store := NewJobStore(pool)
			handler := httpapi.NewHandler(pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			body, err := json.Marshal(jobs.SubmitRequest{Type: "migrate_campaign", Payload: fixture.Payload})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/jobs", bytes.NewReader(body)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "fixture-"+fixture.Name)
			accepted := httptest.NewRecorder()
			handler.ServeHTTP(accepted, request)
			var submitted jobs.Job
			if accepted.Code != http.StatusAccepted || json.Unmarshal(accepted.Body.Bytes(), &submitted) != nil || submitted.Status != jobs.Queued || submitted.Attempts != 0 {
				t.Fatalf("submission: %d %s", accepted.Code, accepted.Body.String())
			}
			w, err := worker.New(store, worker.CampaignProcessor{}, worker.DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			if worked, err := w.RunOnce(ctx); err != nil || !worked {
				t.Fatalf("process: worked=%v error=%v", worked, err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/jobs/"+submitted.ID, nil).WithContext(ctx))
			var job jobs.Job
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &job) != nil {
				t.Fatalf("retrieval: %d %s", response.Code, response.Body.String())
			}
			expected := fixtureOutcome{fixture.Status, 1, fixture.FailureCode, fixture.Result}
			observed := fixtureOutcome{job.Status, job.Attempts, job.FailureCode, job.Result}
			observation, _ := json.Marshal(struct {
				Fixture  string         `json:"fixture"`
				Expected fixtureOutcome `json:"expected"`
				Observed fixtureOutcome `json:"observed"`
			}{fixture.Name, expected, observed})
			t.Logf("HARNESS_OBSERVATION %s", observation)
			var expectedJSON, observedJSON any
			expectedBytes, _ := json.Marshal(expected)
			observedBytes, _ := json.Marshal(observed)
			if json.Unmarshal(expectedBytes, &expectedJSON) != nil || json.Unmarshal(observedBytes, &observedJSON) != nil || !reflect.DeepEqual(expectedJSON, observedJSON) {
				t.Fatalf("expected %s; observed %s", expectedBytes, observedBytes)
			}
			if job.ID != submitted.ID || job.CompletedAt == nil {
				t.Fatal("job identity or terminal timestamp lost")
			}
			historyResponse := httptest.NewRecorder()
			handler.ServeHTTP(historyResponse, httptest.NewRequest(http.MethodGet, "/jobs/"+job.ID+"/attempts", nil).WithContext(ctx))
			var history struct {
				JobID    string         `json:"job_id"`
				Attempts []jobs.Attempt `json:"attempts"`
			}
			if historyResponse.Code != http.StatusOK || json.Unmarshal(historyResponse.Body.Bytes(), &history) != nil || history.JobID != job.ID || len(history.Attempts) != 1 {
				t.Fatalf("history: %d %s", historyResponse.Code, historyResponse.Body.String())
			}
			if history.Attempts[0].Status != string(job.Status) || history.Attempts[0].FailureCode != job.FailureCode || history.Attempts[0].FinishedAt == nil || bytes.Contains(historyResponse.Body.Bytes(), []byte("lease_token")) {
				t.Fatal("history does not agree with terminal state or exposes authority")
			}
			if worked, err := w.RunOnce(ctx); err != nil || worked {
				t.Fatalf("terminal job retried: worked=%v error=%v", worked, err)
			}
		})
	}
}
