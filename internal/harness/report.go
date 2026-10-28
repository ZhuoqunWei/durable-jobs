package harness

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const testPackage = "durable-jobs/internal/database"

type Check struct {
	Test    string  `json:"test"`
	Status  string  `json:"status"`
	Seconds float64 `json:"seconds"`
}

type Result struct {
	Scenario
	Status   string            `json:"status"`
	Seconds  float64           `json:"seconds"`
	Checks   []Check           `json:"checks"`
	Evidence []json.RawMessage `json:"evidence,omitempty"`
}

type Report struct {
	Version   int       `json:"version"`
	StartedAt time.Time `json:"started_at"`
	Seconds   float64   `json:"seconds"`
	Status    string    `json:"status"`
	Command   []string  `json:"command"`
	Results   []Result  `json:"results"`
	Problem   string    `json:"problem,omitempty"`
}

type event struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

// Summarize consumes go test's JSON events, not its human-readable PASS lines.
// Only explicit HARNESS observations are retained; arbitrary test/build output
// may contain connection details, environment values or raw errors.
func Summarize(input io.Reader, selected []Scenario) (Report, error) {
	report := Report{Version: 1, Status: "incomplete", Results: make([]Result, len(selected))}
	index := make(map[string]int, len(selected))
	for i, item := range selected {
		report.Results[i] = Result{Scenario: item, Status: "missing", Checks: []Check{}}
		index[item.Test] = i
	}
	packagePassed, packageFailed := false, false
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var e event
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			return report, errors.New("invalid structured test output")
		}
		if e.Package != testPackage {
			continue
		}
		if e.Test == "" {
			packagePassed = packagePassed || e.Action == "pass"
			packageFailed = packageFailed || e.Action == "fail"
			continue
		}
		root := strings.SplitN(e.Test, "/", 2)[0]
		i, exists := index[root]
		if !exists {
			continue
		}
		r := &report.Results[i]
		switch e.Action {
		case "run":
			if e.Test == root && r.Status == "missing" {
				r.Status = "incomplete"
			}
		case "pass", "fail", "skip":
			if e.Test != root {
				r.Checks = append(r.Checks, Check{Test: e.Test, Status: e.Action, Seconds: e.Elapsed})
			} else {
				r.Seconds = e.Elapsed
			}
			// A skipped child makes its parent incomplete even if Go passes it.
			if e.Action == "fail" {
				r.Status = "fail"
			} else if e.Action == "skip" && r.Status != "fail" {
				r.Status = "skip"
			} else if e.Test == root && r.Status != "fail" && r.Status != "skip" {
				r.Status = "pass"
			}
		case "output":
			// Only our synthetic fixture test emits structured observations.
			if root == "TestCampaignFixtureMatrixIntegration" && len(r.Evidence) < 32 {
				if _, observation, ok := strings.Cut(e.Output, "HARNESS_OBSERVATION "); ok && len(observation) < 8192 && json.Valid([]byte(strings.TrimSpace(observation))) {
					r.Evidence = append(r.Evidence, json.RawMessage(strings.TrimSpace(observation)))
				}
			}
		}
	}
	if scanner.Err() != nil {
		return report, errors.New("structured test output could not be read")
	}
	if packagePassed && len(selected) > 0 {
		report.Status = "pass"
	}
	for _, r := range report.Results {
		if r.Status != "pass" {
			report.Status = "incomplete"
		}
	}
	if packageFailed {
		report.Status = "fail"
	}
	for _, r := range report.Results {
		if r.Status == "fail" {
			report.Status = "fail"
		}
	}
	return report, nil
}
