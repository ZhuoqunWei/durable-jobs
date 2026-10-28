package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

type Runner struct {
	client           commander
	image            string
	executionTimeout time.Duration
}

// ImageID is the immutable ID resolved from the operator's trusted local tag.
// Callers compare it to the persisted pin before executing a queued script.
func (r *Runner) ImageID() string { return r.image }

func createArgs(name, owner, image string) []string {
	args := []string{"create", "--name", name,
		"--label", runnerLabel + "=v1", "--label", ownerLabel + "=" + owner,
		"--pull", "never", "--interactive", "--init", "--network", "none",
		"--read-only", "--user", "65534:65534", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges=true", "--cgroupns", "private", "--ipc", "none",
		"--memory", "64m", "--memory-swap", "64m", "--cpus", "0.5", "--pids-limit", "32",
		"--ulimit", "nofile=64:64", "--ulimit", "core=0:0", "--log-driver", "none",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=8m,mode=1777", "--workdir", "/tmp",
		"--no-healthcheck", "--restart", "no", "--entrypoint", "/usr/local/bin/python3"}
	// Docker client proxy configuration must not inject proxy credentials.
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "ftp_proxy", "all_proxy", "no_proxy"} {
		args = append(args, "--env", key+"=")
	}
	return append(args, image, "-I", "-B", "/opt/durable-jobs/runner.py")
}

// Run returns a safe, bounded outcome. Cleanup uses its own deadline even when
// the caller is canceled. A cleanup failure overrides success, never hides it.
func (r *Runner) Run(ctx context.Context, request Request) (result Result) {
	started := time.Now()
	result.Status, result.CleanupConfirmed = "failed", true
	result.ImageID = r.image
	defer func() { result.ElapsedMS = time.Since(started).Milliseconds() }()
	if request.Validate() != nil {
		result.FailureCode = "invalid_request"
		return result
	}
	if ctx.Err() != nil {
		result.FailureCode = "canceled"
		return result
	}
	sum := sha256.Sum256([]byte(request.Script))
	result.ScriptSHA256 = hex.EncodeToString(sum[:])
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		result.FailureCode = "runner_unavailable"
		return result
	}
	owner := hex.EncodeToString(nonce[:])
	result.Container = "durable-jobs-sandbox-" + owner
	result.CleanupConfirmed = false
	// Install cleanup before create: a lost create acknowledgement may still
	// leave a container. Resolve only our exact name AND both ownership labels.
	defer func() {
		result.CleanupConfirmed = r.cleanup(result.Container, owner)
		if !result.CleanupConfirmed {
			result.Status, result.FailureCode, result.Output = "failed", "cleanup_failed", nil
		}
	}()
	data, err := control(ctx, r.client, createArgs(result.Container, owner, r.image)...)
	id := strings.TrimSpace(string(data))
	if err != nil || !containerID.MatchString(id) {
		result.FailureCode = "container_create_failed"
		return result
	}
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	// This is stdin, not HTML. Escaping '<' into six bytes can inflate a valid
	// 64 KiB input beyond the bootstrap's bounded envelope size.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(request); err != nil || input.Len() > 256<<10 {
		result.FailureCode = "invalid_request"
		return result
	}
	executionCtx, cancel := context.WithTimeout(ctx, r.executionTimeout)
	stdout := &boundedWriter{limit: MaxOutputBytes, cancel: cancel}
	stderr := &boundedWriter{limit: MaxStderrBytes, cancel: cancel}
	runErr := r.client.run(executionCtx, []string{"start", "--attach", "--interactive", id}, &input, stdout, stderr)
	executionErr := executionCtx.Err()
	cancel()
	if stdout.exceeded || stderr.exceeded {
		result.FailureCode = "output_limit"
		return result
	}
	if ctx.Err() != nil {
		result.FailureCode = "canceled"
		return result
	}
	if executionErr != nil {
		result.FailureCode = "time_limit"
		return result
	}
	stateJSON, inspectErr := control(ctx, r.client, "container", "inspect", id, "--format", "{{json .State}}")
	var state struct {
		Running   bool
		OOMKilled bool
		ExitCode  int
		Status    string
	}
	if inspectErr != nil || json.Unmarshal(stateJSON, &state) != nil || state.Running || state.Status != "exited" {
		result.FailureCode = "container_state_unknown"
		return result
	}
	if state.OOMKilled {
		result.FailureCode = "memory_limit"
		return result
	}
	if state.ExitCode != 0 {
		result.FailureCode = "script_failed"
		return result
	}
	if runErr != nil {
		result.FailureCode = "container_attach_failed"
		return result
	}
	if !validObject(stdout.Bytes(), MaxOutputBytes) {
		result.FailureCode = "invalid_output"
		return result
	}
	result.Output = append(json.RawMessage(nil), bytes.TrimSpace(stdout.Bytes())...)
	result.Status = "succeeded"
	return result
}

func (r *Runner) cleanup(name, owner string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	args := []string{"container", "ls", "--all", "--no-trunc",
		"--filter", "name=^/" + name + "$", "--filter", "label=" + runnerLabel + "=v1",
		"--filter", "label=" + ownerLabel + "=" + owner, "--format", "{{.ID}}"}
	data, err := control(ctx, r.client, args...)
	if err != nil {
		return false
	}
	ids := strings.Fields(string(data))
	if len(ids) == 0 {
		return true
	}
	if len(ids) != 1 || !containerID.MatchString(ids[0]) {
		return false
	}
	if _, err := control(ctx, r.client, "container", "rm", "--force", ids[0]); err != nil {
		return false
	}
	data, err = control(ctx, r.client, args...)
	return err == nil && len(bytes.TrimSpace(data)) == 0
}
