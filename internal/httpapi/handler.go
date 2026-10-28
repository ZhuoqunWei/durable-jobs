package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"time"

	"durable-jobs/internal/jobs"
	"durable-jobs/internal/telemetry"
)

// Pinger lets setup tests replace a real database with a tiny test double.
type Pinger interface {
	Ping(context.Context) error
}

type JobStore interface {
	Create(context.Context, jobs.SubmitRequest, string) (jobs.Job, error)
	Get(context.Context, string) (jobs.Job, error)
	ListAttempts(context.Context, string) ([]jobs.Attempt, error)
}

const MaxSubmissionBytes int64 = 64 << 10

var jobIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func NewHandler(db Pinger, store JobStore, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		source, ok := store.(interface {
			Metrics(context.Context) (telemetry.Snapshot, error)
		})
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "metrics unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		snapshot, err := source.Metrics(ctx)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "metrics unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := telemetry.Write(w, snapshot); err != nil {
			writeError(w, http.StatusServiceUnavailable, "metrics unavailable")
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			// Never expose database connection details to an HTTP client.
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "database": "ok"})
	})
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		var key string
		if values := r.Header.Values("Idempotency-Key"); len(values) > 0 {
			// Reject repeated headers, empty values and comma-joined keys instead
			// of silently choosing which operation the client meant to retry.
			if len(values) != 1 || jobs.ValidateIdempotencyKey(values[0]) != nil {
				writeError(w, http.StatusBadRequest, jobs.ErrInvalidIdempotencyKey.Error())
				return
			}
			key = values[0]
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxSubmissionBytes)
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "body exceeds 64 KiB")
			} else {
				writeError(w, http.StatusBadRequest, "cannot read request body")
			}
			return
		}
		request, err := jobs.ParseSubmission(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		job, err := store.Create(ctx, request, key)
		if errors.Is(err, jobs.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, jobs.ErrIdempotencyConflict.Error())
			return
		}
		if errors.Is(err, jobs.ErrInvalidIdempotencyKey) {
			writeError(w, http.StatusBadRequest, jobs.ErrInvalidIdempotencyKey.Error())
			return
		}
		if errors.Is(err, jobs.ErrInvalidPayload) {
			writeError(w, http.StatusBadRequest, "payload is not supported by JSONB storage")
			return
		}
		if err != nil {
			logger.Error("create job failed", "error", err)
			// A timeout can happen after commit. Keep the same key AND input when
			// retrying; a new key could create a second job.
			message := "could not confirm job creation; outcome may be unknown; without an Idempotency-Key a retry can create a duplicate"
			if key != "" {
				message = "could not confirm job creation; retry with the same Idempotency-Key and submission"
			}
			writeError(w, http.StatusServiceUnavailable, message)
			return
		}
		w.Header().Set("Location", "/jobs/"+job.ID)
		logger.Info("job accepted", "job_id", job.ID, "status", job.Status)
		writeJSON(w, http.StatusAccepted, job)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !jobIDPattern.MatchString(id) {
			writeError(w, http.StatusBadRequest, "id must be a UUID")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		job, err := store.Get(ctx, id)
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		if err != nil {
			logger.Error("get job failed", "job_id", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "job storage is unavailable")
			return
		}
		writeJSON(w, http.StatusOK, job)
	})
	mux.HandleFunc("GET /jobs/{id}/attempts", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !jobIDPattern.MatchString(id) {
			writeError(w, http.StatusBadRequest, "id must be a UUID")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		attempts, err := store.ListAttempts(ctx, id)
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		if err != nil {
			logger.Error("get attempt history failed", "job_id", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "job history is unavailable")
			return
		}
		if attempts == nil {
			attempts = []jobs.Attempt{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"job_id": id, "attempts": attempts})
	})
	return mux
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
