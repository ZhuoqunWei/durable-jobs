package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"durable-jobs/internal/harness"
)

func TestListDoesNotRequireDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-list"}, &stdout, &stderr); code != 0 || !bytes.Contains(stdout.Bytes(), []byte("crash-recovery")) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}

func TestCLIRejectsUnknownScenarioAndMissingDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	for _, args := range [][]string{{"-scenario", "typo"}, {"-report-dir", "", "-json"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code == 0 {
			t.Fatalf("false success: stdout=%s stderr=%s", &stdout, &stderr)
		}
	}
}

func TestReportsArePrivateDistinctAndValidJSON(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "reports")
	report := harness.Report{Version: 1, Status: "incomplete", Results: []harness.Result{}}
	first, err := saveReport(directory, report)
	if err != nil {
		t.Fatal(err)
	}
	second, err := saveReport(directory, report)
	if err != nil || first == second {
		t.Fatalf("report overwritten: %s %s %v", first, second, err)
	}
	data, err := os.ReadFile(first)
	if err != nil || !json.Valid(data) {
		t.Fatalf("report invalid: %v", err)
	}
	info, err := os.Stat(first)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("report permissions: %v %v", info, err)
	}
}
