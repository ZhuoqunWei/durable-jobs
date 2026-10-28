package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const controlTimeout = 5 * time.Second

var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var imageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type commander interface {
	run(context.Context, []string, io.Reader, io.Writer, io.Writer) error
}

type dockerCLI struct {
	path string
	host string
	env  []string
}

func (d dockerCLI) run(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer) error {
	if d.host != "" {
		args = append([]string{"--host", d.host}, args...)
	}
	cmd := exec.CommandContext(ctx, d.path, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = d.env, input, stdout, stderr
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

// boundedWriter retains at most limit bytes and cancels the command on overflow.
// stdout/stderr each have their own writer; read them only after cmd.Run returns.
type boundedWriter struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

// Do not embed bytes.Buffer: its promoted ReadFrom method lets io.Copy bypass
// Write and therefore bypass the output limit.
func (w *boundedWriter) Bytes() []byte  { return w.buffer.Bytes() }
func (w *boundedWriter) Len() int       { return w.buffer.Len() }
func (w *boundedWriter) String() string { return w.buffer.String() }

func (w *boundedWriter) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit - w.Len()
	if n > remaining {
		_, _ = w.buffer.Write(p[:remaining])
		w.exceeded = true
		w.cancel()
	} else {
		_, _ = w.buffer.Write(p)
	}
	return n, nil // Discard excess; cancellation and container cleanup stop work.
}

func control(ctx context.Context, client commander, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	stdout := &boundedWriter{limit: 128 << 10, cancel: cancel}
	stderr := &boundedWriter{limit: MaxStderrBytes, cancel: cancel}
	err := client.run(ctx, args, nil, stdout, stderr)
	if err != nil || stdout.exceeded || stderr.exceeded {
		return nil, errors.New("docker_control_failed")
	}
	return stdout.Bytes(), nil
}

func validLocalEndpoint(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "unix" && u.Host == "" && u.User == nil &&
		filepath.IsAbs(u.Path) && u.Path != "/" && u.RawQuery == "" && u.Fragment == ""
}

func pinnedEnvironment(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_TLS", "DOCKER_CERT_PATH", "DOCKER_API_VERSION", "BUILDX_BUILDER", "BUILDKIT_HOST":
			continue
		}
		result = append(result, value)
	}
	return result
}

func localDocker(ctx context.Context) (dockerCLI, error) {
	path, err := exec.LookPath("docker")
	if err != nil {
		return dockerCLI{}, errors.New("docker_unavailable")
	}
	client := dockerCLI{path: path, env: os.Environ()}
	host := os.Getenv("DOCKER_HOST")
	if host == "" || os.Getenv("DOCKER_CONTEXT") != "" {
		data, err := control(ctx, client, "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}")
		if err != nil || json.Unmarshal(data, &host) != nil {
			return dockerCLI{}, errors.New("docker_unavailable")
		}
	}
	if !validLocalEndpoint(host) {
		return dockerCLI{}, errors.New("nonlocal_docker_refused")
	}
	client.host, client.env = host, pinnedEnvironment(os.Environ())
	data, err := control(ctx, client, "info", "--format", "{{json .}}")
	if err != nil {
		return dockerCLI{}, errors.New("docker_unavailable")
	}
	if err := validateDaemon(data); err != nil {
		return dockerCLI{}, err
	}
	return client, nil
}

func validateDaemon(data []byte) error {
	var info struct {
		OSType          string
		SecurityOptions []string
		MemoryLimit     bool
		SwapLimit       bool
		PidsLimit       bool
		CPUCfsQuota     bool
		CPUCfsPeriod    bool
	}
	if json.Unmarshal(data, &info) != nil || info.OSType != "linux" || !info.MemoryLimit || !info.SwapLimit || !info.PidsLimit || !info.CPUCfsQuota || !info.CPUCfsPeriod {
		return errors.New("required_docker_limits_unavailable")
	}
	for _, option := range info.SecurityOptions {
		if option == "name=seccomp,profile=builtin" {
			return nil
		}
	}
	return errors.New("default_seccomp_required")
}

// BuildImage is an explicit setup action. The tiny build context excludes the
// repository and .env. Runtime New/Run never pull or build an image implicitly.
func BuildImage(ctx context.Context, progress io.Writer) error {
	client, err := localDocker(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat("sandbox/Dockerfile"); err != nil {
		return errors.New("run_from_repository_root")
	}
	if err := client.run(ctx, []string{"build", "--builder", "default", "--network", "none", "--tag", ImageTag, "sandbox"}, nil, progress, progress); err != nil {
		return errors.New("sandbox_image_build_failed")
	}
	return nil
}

func New(ctx context.Context) (*Runner, error) {
	client, err := localDocker(ctx)
	if err != nil {
		return nil, err
	}
	return newWithClient(ctx, client)
}

func newWithClient(ctx context.Context, client commander) (*Runner, error) {
	data, err := control(ctx, client, "image", "inspect", ImageTag, "--format", "{{json .}}")
	var info struct {
		ID     string `json:"Id"`
		Config struct {
			Labels  map[string]string
			Volumes map[string]json.RawMessage
		}
	}
	if err != nil || json.Unmarshal(data, &info) != nil || !imageID.MatchString(info.ID) ||
		info.Config.Labels["org.durable-jobs.sandbox.protocol"] != "1" || len(info.Config.Volumes) != 0 {
		return nil, errors.New("sandbox_image_unavailable_or_invalid")
	}
	return &Runner{client: client, image: info.ID, executionTimeout: 3 * time.Second}, nil
}
