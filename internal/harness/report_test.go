package harness

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func testEvents(values ...event) *bytes.Buffer {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for _, value := range values {
		if value.Package == "" {
			value.Package = testPackage
		}
		_ = encoder.Encode(value)
	}
	return &output
}

func TestReportRequiresAllTestsAndPackageCompletion(t *testing.T) {
	selected := []Scenario{{ID: "one", Test: "TestOne", Expected: "one assertion"}, {ID: "two", Test: "TestTwo", Expected: "two assertions"}}
	for _, tc := range []struct {
		name   string
		events []event
		want   string
	}{
		{"complete", []event{{Action: "pass", Test: "TestOne"}, {Action: "pass", Test: "TestTwo"}, {Action: "pass"}}, "pass"},
		{"no_package_result", []event{{Action: "pass", Test: "TestOne"}, {Action: "pass", Test: "TestTwo"}}, "incomplete"},
		{"missing_test", []event{{Action: "pass", Test: "TestOne"}, {Action: "pass"}}, "incomplete"},
		{"interrupted_test", []event{{Action: "pass", Test: "TestOne"}, {Action: "run", Test: "TestTwo"}, {Action: "pass"}}, "incomplete"},
		{"skipped_test", []event{{Action: "pass", Test: "TestOne"}, {Action: "skip", Test: "TestTwo"}, {Action: "pass"}}, "incomplete"},
		{"skipped_child", []event{{Action: "pass", Test: "TestOne"}, {Action: "skip", Test: "TestTwo/child"}, {Action: "pass", Test: "TestTwo"}, {Action: "pass"}}, "incomplete"},
		{"failed_child", []event{{Action: "pass", Test: "TestOne"}, {Action: "fail", Test: "TestTwo/child"}, {Action: "pass", Test: "TestTwo"}, {Action: "pass"}}, "fail"},
		{"failed_package_after_tests", []event{{Action: "pass", Test: "TestOne"}, {Action: "pass", Test: "TestTwo"}, {Action: "fail"}}, "fail"},
		{"wrong_package", []event{{Action: "pass", Test: "TestOne", Package: "other"}, {Action: "pass", Test: "TestTwo"}, {Action: "pass"}}, "incomplete"},
		{"empty", nil, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Summarize(testEvents(tc.events...), selected)
			if err != nil || report.Status != tc.want || len(report.Results) != 2 {
				t.Fatalf("report=%+v error=%v, want %s", report, err, tc.want)
			}
		})
	}
}

func TestReportEvidenceAndTiming(t *testing.T) {
	scenario := Scenarios()[0]
	input := testEvents(
		event{Action: "output", Test: scenario.Test + "/valid", Output: "    file.go:1: HARNESS_OBSERVATION {\"expected\":\"succeeded\",\"observed\":\"succeeded\"}\n"},
		event{Action: "output", Test: scenario.Test, Output: "raw connection password=do-not-save"},
		event{Action: "pass", Test: scenario.Test + "/valid", Elapsed: 0.1},
		event{Action: "pass", Test: scenario.Test, Elapsed: 0.2},
		event{Action: "pass", Elapsed: 1.2},
	)
	report, err := Summarize(input, []Scenario{scenario})
	if err != nil || report.Status != "pass" || len(report.Results[0].Evidence) != 1 || report.Results[0].Seconds != 0.2 || len(report.Results[0].Checks) != 1 {
		t.Fatalf("report=%+v error=%v", report, err)
	}
	data, _ := json.Marshal(report)
	if bytes.Contains(data, []byte("do-not-save")) || !bytes.Contains(data, []byte(`"observed":"succeeded"`)) {
		t.Fatalf("unsafe or missing structured evidence: %s", data)
	}
}

func TestReportRejectsMalformedAndOversizedEvents(t *testing.T) {
	for _, input := range []string{"not JSON", `{"Action":`, strings.Repeat("x", 2<<20)} {
		report, err := Summarize(strings.NewReader(input), Scenarios())
		if err == nil || report.Status == "pass" {
			t.Fatalf("invalid stream accepted: %+v %v", report, err)
		}
	}
}

func TestSelectValidatesManifestAndChoices(t *testing.T) {
	all, err := Select("all")
	if err != nil || len(all) != 10 {
		t.Fatalf("manifest: %+v %v", all, err)
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, scenario := range all {
		if ids[scenario.ID] || names[scenario.Test] || scenario.ID == "" || scenario.Expected == "" || !strings.HasPrefix(scenario.Test, "Test") {
			t.Fatal("invalid or duplicate scenario")
		}
		ids[scenario.ID], names[scenario.Test] = true, true
	}
	for _, value := range []string{"", "made-up", "all,crash-recovery", "crash-recovery,crash-recovery"} {
		if _, err := Select(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	selected, err := Select("crash-recovery, campaign-fixtures")
	if err != nil || len(selected) != 2 || selected[0].ID != "crash-recovery" {
		t.Fatalf("selection: %+v %v", selected, err)
	}
}

func TestHarnessRefusesNonlocalDatabaseConfiguration(t *testing.T) {
	for _, value := range []string{
		"postgres://user:local@127.0.0.1:55432/db?sslmode=disable",
		"postgres://user:local@localhost:55432/db?sslmode=disable",
		"postgres://user:local@[::1]:55432/db?sslmode=disable",
	} {
		if err := ValidateDatabaseURL(value); err != nil {
			t.Fatalf("local rejected: %v", err)
		}
	}
	for _, value := range []string{"", "postgres://%bad", "postgres://user:secret@remote.example/db", "postgres://user:secret@127.0.0.1/db?host=remote.example", "host=localhost,remote.example user=test password=secret dbname=db", "host=/tmp user=test password=secret dbname=db"} {
		err := ValidateDatabaseURL(value)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("must reject without leaking credentials: %v", err)
		}
	}
}

func TestChildEnvironmentAndArguments(t *testing.T) {
	values := childEnvironment([]string{"PATH=/bin", "TEST_DATABASE_URL=old", "PGOPTIONS=unsafe", "GOFLAGS=-exec=other", "DURABLE_JOBS_CRASH_HELPER=1", "DURABLE_JOBS_TEST_SCHEMA=public"}, "local-url")
	joined := strings.Join(values, "\n")
	if joined != "PATH=/bin\nTEST_DATABASE_URL=local-url\nGOFLAGS=" {
		t.Fatalf("child environment: %q", joined)
	}
	args := strings.Join(commandArgs(Scenarios()[:1]), " ")
	if !strings.Contains(args, "-race -count=1 -timeout=90s -run ^(TestCampaignFixtureMatrixIntegration)$") {
		t.Fatalf("arguments: %s", args)
	}
}
