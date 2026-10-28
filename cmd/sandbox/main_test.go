package main

import (
	"bytes"
	"durable-jobs/internal/jobs"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIRejectsMissingAndOversizedFiles(t *testing.T) {
	for _, args := range [][]string{nil, {"-script", "missing", "-input", "missing"}, {"-build-image", "-script", "unexpected"}, {"-job"}, {"-job", "-script", "unexpected", "-input", "missing"}, {"-job", "-build-image"}} {
		var stdout, stderr bytes.Buffer
		if run(args, &stdout, &stderr) != 2 {
			t.Fatalf("arguments accepted: %v", args)
		}
	}
	path := filepath.Join(t.TempDir(), "bounded.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := readBoundedFile(path, 11); err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("valid read: %s %v", data, err)
	}
	if _, err := readBoundedFile(path, 3); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := readBoundedFile(filepath.Dir(path), 100); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestPinnedScriptJobTemplate(t *testing.T) {
	image := "sha256:" + strings.Repeat("a", 64)
	body, err := scriptJob([]byte(`{"synthetic":true}`), image)
	if err != nil {
		t.Fatal(err)
	}
	request, err := jobs.ParseSubmission(body)
	if err != nil || request.Type != "run_script" {
		t.Fatal(request, err)
	}
	if !json.Valid(body) || !bytes.Contains(body, []byte(image)) {
		t.Fatal("missing pin")
	}
	for _, input := range []string{`null`, `[]`, `not json`, `{"x":"` + strings.Repeat("a", 64<<10) + `"}`} {
		if _, err := scriptJob([]byte(input), image); err == nil {
			t.Fatal("accepted invalid/oversized input")
		}
	}
}
