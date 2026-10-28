package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fakeID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeDocker struct {
	calls        [][]string
	exists       bool
	createFails  bool
	removeFails  bool
	inspectFail  bool
	imageJSON    string
	stateJSON    string
	listOverride string
	start        func(context.Context, io.Reader, io.Writer, io.Writer) error
}

func (f *fakeDocker) run(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer) error {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch args[0] {
	case "image":
		value := f.imageJSON
		if value == "" {
			value = `{"Id":"sha256:` + fakeID + `","Config":{"Labels":{"org.durable-jobs.sandbox.protocol":"1"}}}`
		}
		_, _ = io.WriteString(stdout, value)
	case "create":
		f.exists = true
		if f.createFails {
			return errors.New("lost create acknowledgement")
		}
		_, _ = io.WriteString(stdout, fakeID+"\n")
	case "start":
		if f.start != nil {
			return f.start(ctx, input, stdout, stderr)
		}
		_, _ = io.WriteString(stdout, `{"ok":true}`)
	case "container":
		switch args[1] {
		case "inspect":
			if f.inspectFail {
				return errors.New("inspect failed")
			}
			value := f.stateJSON
			if value == "" {
				value = `{"Status":"exited","ExitCode":0,"Running":false,"OOMKilled":false}`
			}
			_, _ = io.WriteString(stdout, value)
		case "ls":
			if ctx.Err() != nil {
				return errors.New("cleanup incorrectly inherited canceled context")
			}
			if f.listOverride != "" {
				_, _ = io.WriteString(stdout, f.listOverride)
				return nil
			}
			if f.exists {
				_, _ = io.WriteString(stdout, fakeID+"\n")
			}
		case "rm":
			if f.removeFails {
				return errors.New("remove failed")
			}
			if !reflect.DeepEqual(args, []string{"container", "rm", "--force", fakeID}) {
				return errors.New("unexpected removal target")
			}
			f.exists = false
		default:
			return fmt.Errorf("unexpected container command %q", args)
		}
	default:
		return fmt.Errorf("unexpected command %q", args)
	}
	return nil
}

func TestRequestAndOutputValidation(t *testing.T) {
	valid := Request{Script: "def transform(record): return record", Input: json.RawMessage(`{"ok":true}`)}
	if valid.Validate() != nil {
		t.Fatal("valid request rejected")
	}
	for _, input := range []string{"", "null", "[]", `{"x":NaN}`, `{} {}`, string([]byte{'{', '"', 0xff, '"', ':', '1', '}'}), `{"x":"` + strings.Repeat("a", MaxInputBytes) + `"}`} {
		request := valid
		request.Input = json.RawMessage(input)
		if request.Validate() == nil {
			t.Fatal("invalid input accepted")
		}
	}
	for _, script := range []string{"", strings.Repeat("a", MaxScriptBytes+1), string([]byte{0xff})} {
		request := valid
		request.Script = script
		if request.Validate() == nil {
			t.Fatal("invalid script accepted")
		}
	}
	if validObject([]byte(`{"ok":true}`), 3) {
		t.Fatal("output bound ignored")
	}
}

func TestRunnerOutcomesAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		edit func(*fakeDocker)
	}{
		{"success", "", func(f *fakeDocker) {}},
		{"uncertain_create", "container_create_failed", func(f *fakeDocker) { f.createFails = true }},
		{"script_exception", "script_failed", func(f *fakeDocker) { f.stateJSON = `{"Status":"exited","ExitCode":1}` }},
		{"oom", "memory_limit", func(f *fakeDocker) { f.stateJSON = `{"Status":"exited","ExitCode":137,"OOMKilled":true}` }},
		{"still_running", "container_state_unknown", func(f *fakeDocker) { f.stateJSON = `{"Status":"running","Running":true}` }},
		{"inspect_failure", "container_state_unknown", func(f *fakeDocker) { f.inspectFail = true }},
		{"attach_failure", "container_attach_failed", func(f *fakeDocker) {
			f.start = func(context.Context, io.Reader, io.Writer, io.Writer) error { return errors.New("attach broke") }
		}},
		{"timeout", "time_limit", func(f *fakeDocker) {
			f.start = func(ctx context.Context, _ io.Reader, _, _ io.Writer) error { <-ctx.Done(); return ctx.Err() }
		}},
		{"invalid_output", "invalid_output", func(f *fakeDocker) {
			f.start = func(_ context.Context, _ io.Reader, out, _ io.Writer) error {
				_, _ = io.WriteString(out, "[]")
				return nil
			}
		}},
		{"stdout_limit", "output_limit", func(f *fakeDocker) {
			f.start = func(_ context.Context, _ io.Reader, out, _ io.Writer) error {
				_, _ = io.WriteString(out, strings.Repeat("x", MaxOutputBytes+1))
				return nil
			}
		}},
		{"stderr_limit", "output_limit", func(f *fakeDocker) {
			f.start = func(_ context.Context, _ io.Reader, _, errout io.Writer) error {
				_, _ = io.WriteString(errout, strings.Repeat("x", MaxStderrBytes+1))
				return nil
			}
		}},
		{"cleanup_failure_overrides_success", "cleanup_failed", func(f *fakeDocker) { f.removeFails = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDocker{}
			tc.edit(fake)
			runner, err := newWithClient(context.Background(), fake)
			if err != nil {
				t.Fatal(err)
			}
			runner.executionTimeout = 10 * time.Millisecond
			result := runner.Run(context.Background(), Request{Script: "def transform(r): return r", Input: json.RawMessage(`{}`)})
			if result.FailureCode != tc.code || result.CleanupConfirmed == fake.removeFails || fake.exists != fake.removeFails {
				t.Fatalf("result=%+v remaining=%v want code %s", result, fake.exists, tc.code)
			}
			if tc.code == "" && (result.Status != "succeeded" || string(result.Output) != `{"ok":true}`) {
				t.Fatalf("success result: %+v", result)
			}
			if tc.code != "" && (result.Status != "failed" || len(result.Output) != 0) {
				t.Fatalf("failure exposed output: %+v", result)
			}
			for _, args := range fake.calls {
				if args[0] == "container" && args[1] == "ls" {
					joined := strings.Join(args, " ")
					if !strings.Contains(joined, "name=^/"+result.Container+"$") || !strings.Contains(joined, "label="+ownerLabel+"=") || !strings.Contains(joined, "label="+runnerLabel+"=v1") {
						t.Fatalf("cleanup lacks exact ownership filters: %v", args)
					}
				}
			}
		})
	}
}

func TestCancellationAndInputDoNotBypassCleanup(t *testing.T) {
	fake := &fakeDocker{}
	runner, _ := newWithClient(context.Background(), fake)
	ctx, cancel := context.WithCancel(context.Background())
	fake.start = func(_ context.Context, _ io.Reader, _, _ io.Writer) error { cancel(); return context.Canceled }
	result := runner.Run(ctx, Request{Script: "def transform(r): return r", Input: json.RawMessage(`{}`)})
	if result.FailureCode != "canceled" || !result.CleanupConfirmed || fake.exists {
		t.Fatalf("cancellation: %+v", result)
	}
	before := len(fake.calls)
	result = runner.Run(context.Background(), Request{})
	if result.FailureCode != "invalid_request" || !result.CleanupConfirmed || len(fake.calls) != before {
		t.Fatalf("invalid request touched Docker: %+v", result)
	}
}

func TestBoundaryArgumentsAreMandatory(t *testing.T) {
	args := createArgs("name", "owner", "sha256:"+fakeID)
	joined := strings.Join(args, " ")
	for _, required := range []string{"--pull never", "--network none", "--read-only", "--user 65534:65534", "--cap-drop ALL", "--security-opt no-new-privileges=true", "--memory 64m", "--memory-swap 64m", "--cpus 0.5", "--pids-limit 32", "--log-driver none", "--ipc none", "--cgroupns private", "--tmpfs /tmp:rw,noexec,nosuid,nodev,size=8m", "--entrypoint /usr/local/bin/python3", "--env HTTPS_PROXY=", "sha256:" + fakeID + " -I -B /opt/durable-jobs/runner.py"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q in %s", required, joined)
		}
	}
	for _, forbidden := range []string{"--privileged", "--mount", "--volume", "--volumes-from", "--env-file", "--publish", "--use-api-socket"} {
		for _, arg := range args {
			if arg == forbidden {
				t.Fatalf("unsafe option: %s", arg)
			}
		}
	}
}

func TestEndpointAndDaemonGuards(t *testing.T) {
	if !validLocalEndpoint("unix:///Users/demo/.docker/run/docker.sock") {
		t.Fatal("local endpoint refused")
	}
	for _, endpoint := range []string{"tcp://localhost:2375", "ssh://remote", "unix://remote/tmp/socket", "unix://", "unix:///", "unix:///tmp/socket?x=1", "unix:///tmp/socket#fragment"} {
		if validLocalEndpoint(endpoint) {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	info := map[string]any{"OSType": "linux", "SecurityOptions": []string{"name=seccomp,profile=builtin"}, "MemoryLimit": true, "SwapLimit": true, "PidsLimit": true, "CpuCfsQuota": true, "CpuCfsPeriod": true}
	data, _ := json.Marshal(info)
	if validateDaemon(data) != nil {
		t.Fatal("supported daemon refused")
	}
	for key := range info {
		missing := make(map[string]any, len(info))
		for name, value := range info {
			missing[name] = value
		}
		delete(missing, key)
		data, _ := json.Marshal(missing)
		if validateDaemon(data) == nil {
			t.Fatalf("missing %s accepted", key)
		}
	}
	env := pinnedEnvironment([]string{"PATH=/bin", "DOCKER_HOST=tcp://remote", "DOCKER_CONTEXT=cloud", "DOCKER_API_VERSION=1.1", "BUILDX_BUILDER=remote", "BUILDKIT_HOST=tcp://remote"})
	if !reflect.DeepEqual(env, []string{"PATH=/bin"}) {
		t.Fatalf("unpinned environment: %v", env)
	}
}

func TestCleanupRefusesAmbiguousOrInvalidTargets(t *testing.T) {
	for _, listing := range []string{"not-an-id", fakeID + "\n" + strings.Repeat("b", 64)} {
		fake := &fakeDocker{exists: true, listOverride: listing}
		runner, err := newWithClient(context.Background(), fake)
		if err != nil {
			t.Fatal(err)
		}
		if runner.cleanup("owned-name", "owner") {
			t.Fatal("ambiguous cleanup accepted")
		}
		for _, args := range fake.calls {
			if args[0] == "container" && args[1] == "rm" {
				t.Fatal("removed ambiguous target")
			}
		}
	}
}

func TestImageGuardAndBoundedWriter(t *testing.T) {
	for _, value := range []string{`{}`, `{"Id":"tag"}`, `{"Id":"sha256:` + fakeID + `","Config":{"Labels":{"org.durable-jobs.sandbox.protocol":"1"},"Volumes":{"/data":{}}}}`} {
		if _, err := newWithClient(context.Background(), &fakeDocker{imageJSON: value}); err == nil {
			t.Fatal("untrusted or volume-declaring image accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &boundedWriter{limit: 4, cancel: cancel}
	if _, bypass := any(w).(io.ReaderFrom); bypass {
		t.Fatal("io.Copy must not bypass the bounded Write method")
	}
	if _, bypass := any(w).(io.StringWriter); bypass {
		t.Fatal("io.WriteString must not bypass the bounded Write method")
	}
	_, _ = w.Write([]byte("abcd"))
	if ctx.Err() != nil || w.exceeded {
		t.Fatal("exact limit rejected")
	}
	_, _ = w.Write([]byte("efgh"))
	_, _ = w.Write([]byte("more"))
	if w.String() != "abcd" || !w.exceeded || ctx.Err() == nil {
		t.Fatal("overflow not bounded and canceled")
	}
}
