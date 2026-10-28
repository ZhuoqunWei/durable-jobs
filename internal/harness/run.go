package harness

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ValidateDatabaseURL restricts this demo harness to loopback PostgreSQL hosts,
// including any libpq/pgx fallback hosts. Credentials never enter the report.
func ValidateDatabaseURL(value string) error {
	if value == "" {
		return errors.New("DATABASE_URL is required; run make harness from the project folder")
	}
	cfg, err := pgx.ParseConfig(value)
	if err != nil {
		return errors.New("DATABASE_URL is invalid (connection details omitted)")
	}
	hosts := []string{cfg.Host}
	for _, fallback := range cfg.Fallbacks {
		hosts = append(hosts, fallback.Host)
	}
	for _, host := range hosts {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("the harness only supports local loopback PostgreSQL; remote hosts and Unix sockets are refused")
		}
	}
	return nil
}

func commandArgs(selected []Scenario) []string {
	names := make([]string, len(selected))
	for i, scenario := range selected {
		names[i] = regexp.QuoteMeta(scenario.Test)
	}
	return []string{"test", "-json", "-race", "-count=1", "-timeout=90s", "-run", "^(" + strings.Join(names, "|") + ")$", "./internal/database"}
}

func childEnvironment(values []string, databaseURL string) []string {
	result := make([]string, 0, len(values)+2)
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "TEST_DATABASE_URL", "PGOPTIONS", "GOFLAGS", "DURABLE_JOBS_CRASH_HELPER", "DURABLE_JOBS_TEST_SCHEMA":
			continue
		}
		result = append(result, value)
	}
	return append(result, "TEST_DATABASE_URL="+databaseURL, "GOFLAGS=")
}

// Run executes only the selected checked-in tests, each of which owns an
// isolated PostgreSQL schema. It never points a worker at the application queue.
func Run(ctx context.Context, root, databaseURL string, selected []Scenario) (Report, error) {
	started := time.Now().UTC()
	report := Report{Version: 1, StartedAt: started, Status: "incomplete", Results: []Result{}}
	if err := ValidateDatabaseURL(databaseURL); err != nil {
		return report, err
	}
	if len(selected) == 0 {
		return report, errors.New("at least one scenario is required")
	}
	// The harness is repository-local: tests and their fixture files are inputs.
	module, err := os.ReadFile(root + "/go.mod")
	if err != nil || !strings.HasPrefix(string(module), "module durable-jobs\n") {
		return report, errors.New("run the harness from the durable-jobs repository root")
	}
	args := commandArgs(selected)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.Env = childEnvironment(os.Environ(), databaseURL)
	// Interrupt lets Go's test timeout/cleanup run on user cancellation. A later
	// forced exit can still leave an isolated schema; never sweep schemas by glob.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = io.Discard // Never persist raw build/connection diagnostics.
	output, err := cmd.StdoutPipe()
	if err != nil {
		return report, errors.New("cannot read test process output")
	}
	if err := cmd.Start(); err != nil {
		return report, errors.New("cannot start Go tests; verify Go is available")
	}
	parsed, parseErr := Summarize(output, selected)
	if parseErr != nil {
		_ = cmd.Process.Kill() // Only the exact child started above.
	}
	runErr := cmd.Wait()
	report = parsed
	report.StartedAt = started
	report.Seconds = time.Since(started).Seconds()
	report.Command = append([]string{"go"}, args...)
	if ctx.Err() != nil {
		report.Status = "incomplete"
		report.Problem = "run interrupted; some isolated test schemas may require inspection"
	} else if parseErr != nil {
		report.Status = "incomplete"
		report.Problem = parseErr.Error()
	} else if runErr != nil {
		report.Status = "fail"
		report.Problem = "Go tests exited unsuccessfully; use make integration to inspect diagnostics locally"
	} else if report.Status != "pass" {
		report.Problem = "one or more required tests or subtests were missing, skipped or unfinished"
	}
	if report.Status != "pass" {
		return report, errors.New("harness did not pass")
	}
	return report, nil
}
