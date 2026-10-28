package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeDatabase struct {
	err error
}

func (db fakeDatabase) Ping(context.Context) error { return db.err }

func TestHealthAndReadiness(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		dbError    error
		wantCode   int
		wantStatus string
	}{
		{"healthy process", "/healthz", nil, http.StatusOK, "ok"},
		{"health does not depend on database", "/healthz", errors.New("offline"), http.StatusOK, "ok"},
		{"database ready", "/readyz", nil, http.StatusOK, "ready"},
		{"database offline", "/readyz", errors.New("private connection detail"), http.StatusServiceUnavailable, "not_ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			NewHandler(fakeDatabase{tt.dbError}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(response, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if response.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantCode)
			}
			if response.Header().Get("Content-Type") != "application/json" {
				t.Fatal("expected JSON content type")
			}
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["status"] != tt.wantStatus {
				t.Fatalf("status body = %q, want %q", body["status"], tt.wantStatus)
			}
			if strings.Contains(response.Body.String(), "private connection detail") {
				t.Fatal("response leaked database error")
			}
		})
	}
}

func TestRouting(t *testing.T) {
	tests := []struct {
		method string
		path   string
		code   int
	}{
		{http.MethodPost, "/jobs", http.StatusUnsupportedMediaType},
		{http.MethodGet, "/missing", http.StatusNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			NewHandler(fakeDatabase{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(response, httptest.NewRequest(tt.method, tt.path, nil))
			if response.Code != tt.code {
				t.Fatalf("status = %d, want %d", response.Code, tt.code)
			}
		})
	}
}
