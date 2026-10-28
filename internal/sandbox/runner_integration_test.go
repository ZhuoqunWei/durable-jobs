package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Verify the daemon's actual created-container configuration before any of the
// resource-abuse probes execute. This wrapper is only used in opt-in tests.
type auditedDocker struct {
	commander
	t *testing.T
}

func (d auditedDocker) run(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer) error {
	if args[0] == "start" {
		data, err := control(ctx, d.commander, "container", "inspect", args[len(args)-1], "--format", "{{json .}}")
		var actual struct {
			Config struct {
				User string
				Env  []string
			}
			HostConfig struct {
				NetworkMode, IpcMode, CgroupnsMode       string
				ReadonlyRootfs, Privileged               bool
				Memory, MemorySwap, PidsLimit            int64
				NanoCPUs                                 int64 `json:"NanoCpus"`
				CapDrop, SecurityOpt, Binds, VolumesFrom []string
				Mounts                                   []json.RawMessage
				PortBindings                             map[string]json.RawMessage
				Tmpfs                                    map[string]string
				LogConfig                                struct{ Type string }
			}
		}
		if err != nil || json.Unmarshal(data, &actual) != nil {
			d.t.Fatal("cannot audit created container")
		}
		h := actual.HostConfig
		if actual.Config.User != "65534:65534" || h.NetworkMode != "none" || h.IpcMode != "none" || h.CgroupnsMode != "private" || !h.ReadonlyRootfs || h.Privileged || h.Memory != 64<<20 || h.MemorySwap != 64<<20 || h.PidsLimit != 32 || h.NanoCPUs != 500000000 || h.LogConfig.Type != "none" || len(h.Binds)+len(h.VolumesFrom)+len(h.Mounts)+len(h.PortBindings) != 0 || !reflect.DeepEqual(h.CapDrop, []string{"ALL"}) || !strings.Contains(strings.Join(h.SecurityOpt, " "), "no-new-privileges") || !strings.Contains(h.Tmpfs["/tmp"], "size=8m") || !strings.Contains(h.Tmpfs["/tmp"], "noexec") {
			d.t.Fatal("daemon did not apply required isolation/resource configuration; refusing to execute probe")
		}
		for _, value := range actual.Config.Env {
			if strings.HasPrefix(value, "DATABASE_URL=") || strings.HasPrefix(value, "DURABLE_JOBS_TEST_SECRET=") {
				d.t.Fatal("host application environment leaked into container")
			}
		}
	}
	return d.commander.run(ctx, args, input, stdout, stderr)
}

func integrationRunner(t *testing.T) *Runner {
	t.Helper()
	if os.Getenv("SANDBOX_INTEGRATION") != "1" {
		t.Skip("explicit Docker test: make sandbox-image, then make sandbox-check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner, err := New(ctx)
	if err != nil {
		t.Fatalf("sandbox prerequisite missing: %v", err)
	}
	runner.client = auditedDocker{runner.client, t}
	return runner
}

func TestSandboxIdentityIntegration(t *testing.T) {
	runner := integrationRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result := runner.Run(ctx, Request{Script: "def transform(record):\n    return record\n", Input: json.RawMessage(`{"hello":"sandbox"}`)})
	if result.Status != "succeeded" || !result.CleanupConfirmed {
		t.Fatalf("identity: %+v", result)
	}
}

func assertRemoved(t *testing.T, runner *Runner, result Result) {
	t.Helper()
	if !result.CleanupConfirmed || result.Container == "" {
		t.Fatalf("cleanup unconfirmed: %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := control(ctx, runner.client, "container", "ls", "--all", "--filter", "name=^/"+result.Container+"$", "--format", "{{.ID}}")
	if err != nil || len(strings.TrimSpace(string(data))) != 0 {
		t.Fatal("owned container remains after runner returned")
	}
}

func TestSandboxCampaignFixturesIntegration(t *testing.T) {
	runner := integrationRunner(t)
	source, err := os.ReadFile("../../examples/transform-campaign.py")
	if err != nil {
		t.Fatal(err)
	}
	fixtureData, err := os.ReadFile("../database/testdata/campaign-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
		Status  string          `json:"status"`
		Result  json.RawMessage `json:"result"`
	}
	if json.Unmarshal(fixtureData, &fixtures) != nil || len(fixtures) != 6 {
		t.Fatal("invalid synthetic fixture matrix")
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			result := runner.Run(ctx, Request{Script: string(source), Input: fixture.Payload})
			assertRemoved(t, runner, result)
			if result.Status != fixture.Status {
				t.Fatalf("fixture %s: %+v", fixture.Name, result)
			}
			if fixture.Status == "failed" {
				if result.FailureCode != "script_failed" || len(result.Output) != 0 {
					t.Fatalf("invalid campaign not rejected: %+v", result)
				}
				return
			}
			var expected, observed any
			if json.Unmarshal(fixture.Result, &expected) != nil || json.Unmarshal(result.Output, &observed) != nil || !reflect.DeepEqual(expected, observed) {
				t.Fatalf("expected=%s observed=%s", fixture.Result, result.Output)
			}
		})
	}
}

func TestSandboxIsolationAndResourceLimitsIntegration(t *testing.T) {
	t.Setenv("DURABLE_JOBS_TEST_SECRET", "synthetic-host-only-canary")
	runner := integrationRunner(t)
	canary := filepath.Join(t.TempDir(), "host-only-canary")
	if err := os.WriteFile(canary, []byte("synthetic host data"), 0600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"host_canary": canary})
	for _, tc := range []struct {
		name, script, failure string
	}{
		{"identity_and_privileges", `import os
def transform(record):
    status = dict(line.split(":", 1) for line in open("/proc/self/status") if ":" in line)
    assert os.getuid() == 65534 and os.getgid() == 65534
    assert int(status["CapEff"].strip(), 16) == 0
    assert status["NoNewPrivs"].strip() == "1"
    assert status["Seccomp"].strip() == "2"
    assert "DURABLE_JOBS_TEST_SECRET" not in os.environ
    assert "DATABASE_URL" not in os.environ
    assert not os.path.exists(record["host_canary"])
    assert not os.path.exists("/var/run/docker.sock")
    return {"checked": True}
`, ""},
		{"network_disabled", `import errno, socket
def transform(record):
    # The kernel can expose inactive tunnel devices even with network=none.
    # Check usable links/routes and actual connection behavior, not name count.
    for _, name in socket.if_nameindex():
        flags = int(open("/sys/class/net/" + name + "/flags").read(), 16)
        assert name == "lo" or not (flags & 1)
    assert len(open("/proc/net/route").read().strip().splitlines()) == 1
    try:
        socket.create_connection(("192.0.2.1", 9), timeout=0.2)
    except OSError as error:
        assert error.errno == errno.ENETUNREACH
        return {"checked": True}
    raise RuntimeError("unexpected external network")
`, ""},
		{"read_only_root_and_scratch", `def transform(record):
    with open("/tmp/ephemeral", "w") as handle:
        handle.write("synthetic")
    try:
        open("/etc/runner-write-probe", "w")
    except OSError:
        return {"checked": True}
    raise RuntimeError("root write unexpectedly succeeded")
`, ""},
		{"scratch_size", `def transform(record):
    try:
        with open("/tmp/oversized", "wb") as handle:
            for _ in range(10):
                handle.write(b"x" * (1024 * 1024))
    except OSError:
        return {"checked": True}
    raise RuntimeError("scratch bound missing")
`, ""},
		{"process_count", `import os, signal, time
def transform(record):
    children = []
    blocked = False
    try:
        for _ in range(64):
            try:
                pid = os.fork()
            except BlockingIOError:
                blocked = True
                break
            if pid == 0:
                time.sleep(10)
                os._exit(0)
            children.append(pid)
    finally:
        for pid in children:
            os.kill(pid, signal.SIGKILL)
        for pid in children:
            os.waitpid(pid, 0)
    assert blocked and len(children) <= 30
    return {"checked": True}
`, ""},
		{"infinite_loop", "def transform(record):\n    while True: pass\n", "time_limit"},
		{"memory_limit", "def transform(record):\n    data = bytearray(256 * 1024 * 1024)\n    return {}\n", "memory_limit"},
		{"stdout_flood", "import os\ndef transform(record):\n    while True: os.write(1, b'x' * 4096)\n", "output_limit"},
		{"stderr_flood", "import os\ndef transform(record):\n    while True: os.write(2, b'x' * 4096)\n", "output_limit"},
		{"non_json_stdout", "import os\ndef transform(record):\n    os.write(1, b'not JSON')\n    os._exit(0)\n", "invalid_output"},
		{"array_stdout", "import os\ndef transform(record):\n    os.write(1, b'[]')\n    os._exit(0)\n", "invalid_output"},
		{"non_object_return", "def transform(record): return [1, 2, 3]\n", "script_failed"},
		{"oversized_return", "def transform(record): return {'x': 'x' * (65 * 1024)}\n", "script_failed"},
		{"exception_sanitized", "def transform(record): raise RuntimeError('synthetic-secret-not-to-return')\n", "script_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			result := runner.Run(ctx, Request{Script: tc.script, Input: input})
			assertRemoved(t, runner, result)
			if result.FailureCode != tc.failure {
				t.Fatalf("expected %q: %+v", tc.failure, result)
			}
			if tc.failure == "" {
				if result.Status != "succeeded" || string(result.Output) != `{"checked":true}` {
					t.Fatalf("probe did not prove its assertions: %+v", result)
				}
			} else if result.Status != "failed" || len(result.Output) != 0 {
				t.Fatalf("failed probe exposed untrusted output: %+v", result)
			}
		})
	}
}

func TestSandboxInputSizeBoundaryIntegration(t *testing.T) {
	runner := integrationRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ending := "\ndef transform(record): return {'length': len(record['x'])}\n"
	source := "#" + strings.Repeat("<", MaxScriptBytes-len(ending)-1) + ending
	input := json.RawMessage(`{"x":"` + strings.Repeat("<", MaxInputBytes-8) + `"}`)
	result := runner.Run(ctx, Request{Script: source, Input: input})
	assertRemoved(t, runner, result)
	var output struct {
		Length int `json:"length"`
	}
	if result.Status != "succeeded" || json.Unmarshal(result.Output, &output) != nil || output.Length != MaxInputBytes-8 {
		t.Fatalf("valid maximum-size input failed: %+v", result)
	}
}

type cancelOnStart struct {
	commander
	cancel context.CancelFunc
}

func (c cancelOnStart) run(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer) error {
	if args[0] == "start" {
		timer := time.AfterFunc(200*time.Millisecond, c.cancel)
		defer timer.Stop()
	}
	return c.commander.run(ctx, args, input, stdout, stderr)
}

func TestSandboxCancellationRemovesContainerIntegration(t *testing.T) {
	runner := integrationRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.client = cancelOnStart{runner.client, cancel}
	result := runner.Run(ctx, Request{Script: "def transform(record):\n    while True: pass\n", Input: json.RawMessage(`{}`)})
	assertRemoved(t, runner, result)
	if result.FailureCode != "canceled" {
		t.Fatalf("caller cancellation: %+v", result)
	}
}
