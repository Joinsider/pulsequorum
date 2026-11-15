// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package harness runs UptimeCluster voters as containers and injects faults
// into them: kill, stop, isolate, latency. It drives the container runtime
// through its CLI and uses only flags that podman and docker both understand.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Supported container runtimes.
const (
	RuntimePodman = "podman"
	RuntimeDocker = "docker"
)

// Timeouts of runtime calls. Image builds are slow; everything else is short.
const (
	defaultCallTimeout = 2 * time.Minute
	buildTimeout       = 30 * time.Minute
	maxStderrInError   = 400
)

// execFunc runs bin with args and returns stdout and stderr.
type execFunc func(ctx context.Context, bin string, args []string) (stdout, stderr []byte, err error)

// Runtime wraps the container CLI (podman or docker).
type Runtime struct {
	// Name is RuntimePodman or RuntimeDocker.
	Name string
	// Bin is the executable (the name, or a full path after LookPath).
	Bin string
	// Log receives debug messages; nil discards them.
	Log *slog.Logger

	exec execFunc
}

// CommandError is a failed runtime call. Stderr is truncated; the command
// contains only flags and names, never secrets.
type CommandError struct {
	Command string
	Stderr  string
	Err     error
}

func (e *CommandError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("%s: %v", e.Command, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Command, e.Err, e.Stderr)
}

// Unwrap returns the underlying exec error.
func (e *CommandError) Unwrap() error { return e.Err }

// IsNotFound reports whether err is a runtime "no such ..." answer.
func IsNotFound(err error) bool {
	var ce *CommandError
	if !errors.As(err, &ce) {
		return false
	}
	s := strings.ToLower(ce.Stderr)
	return strings.Contains(s, "no such") || strings.Contains(s, "not found")
}

// ResolveRuntime returns the runtime to use. An empty flag selects podman if
// it is installed and docker otherwise. lookPath is exec.LookPath in
// production.
func ResolveRuntime(flag string, lookPath func(string) (string, error)) (string, error) {
	switch flag {
	case "":
		if _, err := lookPath(RuntimePodman); err == nil {
			return RuntimePodman, nil
		}
		if _, err := lookPath(RuntimeDocker); err == nil {
			return RuntimeDocker, nil
		}
		return "", errors.New("no container runtime found: install podman or docker")
	case RuntimePodman, RuntimeDocker:
		if _, err := lookPath(flag); err != nil {
			return "", fmt.Errorf("container runtime %q not found in PATH: %w", flag, err)
		}
		return flag, nil
	default:
		return "", fmt.Errorf("-runtime must be %q or %q, got %q", RuntimePodman, RuntimeDocker, flag)
	}
}

// NewRuntime resolves name (see ResolveRuntime) and returns a runtime that
// runs the real CLI.
func NewRuntime(name string, log *slog.Logger) (*Runtime, error) {
	resolved, err := ResolveRuntime(name, exec.LookPath)
	if err != nil {
		return nil, err
	}
	bin, err := exec.LookPath(resolved)
	if err != nil {
		return nil, err
	}
	return &Runtime{Name: resolved, Bin: bin, Log: log, exec: defaultExec}, nil
}

// newRuntimeWithExec returns a runtime that uses fn instead of the CLI. Used
// by the unit tests.
func newRuntimeWithExec(name string, fn execFunc) *Runtime {
	return &Runtime{Name: name, Bin: name, exec: fn}
}

func defaultExec(ctx context.Context, bin string, args []string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- fixed runtime binary, arguments built by this package
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// run executes the runtime with args under timeout and returns stdout and
// stderr. The returned error is a *CommandError.
func (r *Runtime) run(ctx context.Context, timeout time.Duration, args ...string) (string, string, error) {
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if r.Log != nil {
		r.Log.Debug("runtime call", "cmd", r.Name+" "+strings.Join(args, " "))
	}
	stdout, stderr, err := r.exec(cctx, r.Bin, args)
	if err != nil {
		return string(stdout), string(stderr), &CommandError{
			Command: r.Name + " " + strings.Join(args, " "),
			Stderr:  truncate(strings.TrimSpace(string(stderr)), maxStderrInError),
			Err:     err,
		}
	}
	return string(stdout), string(stderr), nil
}

// Cmd runs the runtime and returns trimmed stdout.
func (r *Runtime) Cmd(ctx context.Context, args ...string) (string, error) {
	out, _, err := r.run(ctx, defaultCallTimeout, args...)
	return strings.TrimSpace(out), err
}

// Check verifies that the runtime answers (for example that the podman
// machine is running).
func (r *Runtime) Check(ctx context.Context) error {
	if _, err := r.Cmd(ctx, "version", "--format", "{{.Client.Version}}"); err != nil {
		return fmt.Errorf("%s is not usable: %w", r.Name, err)
	}
	return nil
}

// ImageExists reports whether the local image store has image.
func (r *Runtime) ImageExists(ctx context.Context, image string) (bool, error) {
	_, _, err := r.run(ctx, defaultCallTimeout, "image", "inspect", image)
	if err == nil {
		return true, nil
	}
	// podman answers "failed to find image" for a missing image, docker "no such image".
	var ce *CommandError
	if IsNotFound(err) || (errors.As(err, &ce) && strings.Contains(strings.ToLower(ce.Stderr), "failed to find image")) {
		return false, nil
	}
	return false, err
}

// BuildSpec describes one image build.
type BuildSpec struct {
	Tag       string
	File      string // Containerfile or Dockerfile, relative to Context or absolute
	Context   string // build context directory
	BuildArgs map[string]string
}

// args returns the argument list of the build (deterministic order). Missing
// base images are pulled by the build itself.
func (b BuildSpec) args() []string {
	args := []string{"build", "-t", b.Tag}
	if b.File != "" {
		args = append(args, "-f", b.File)
	}
	keys := make([]string, 0, len(b.BuildArgs))
	for k := range b.BuildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+b.BuildArgs[k])
	}
	return append(args, b.Context)
}

// Build builds an image. It waits up to 30 minutes.
func (r *Runtime) Build(ctx context.Context, spec BuildSpec) error {
	if spec.Tag == "" || spec.Context == "" {
		return errors.New("build needs a tag and a context directory")
	}
	_, _, err := r.run(ctx, buildTimeout, spec.args()...)
	return err
}

// RunSpec describes one container to create and start.
type RunSpec struct {
	Name             string
	Image            string
	Network          string
	NetworkContainer string // "container:<name>" network mode; overrides Network
	User             string
	Env              map[string]string
	Publish          []string // host:port:container entries
	Volumes          []string // name:/path or /host:/path[:ro]
	CapAdd           []string
	AddHost          []string
	Command          []string
	Detach           bool
	Rm               bool
}

// RunArgs returns the argument list of the run (deterministic order). Only
// flags that both podman and docker accept are used.
func (s RunSpec) RunArgs() []string {
	args := []string{"run", "--pull=never", "--name", s.Name}
	switch {
	case s.NetworkContainer != "":
		args = append(args, "--network", "container:"+s.NetworkContainer)
	case s.Network != "":
		args = append(args, "--network", s.Network)
	}
	if s.Detach {
		args = append(args, "-d")
	}
	if s.Rm {
		args = append(args, "--rm")
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	for _, p := range s.Publish {
		args = append(args, "-p", p)
	}
	for _, v := range s.Volumes {
		args = append(args, "-v", v)
	}
	for _, c := range s.CapAdd {
		args = append(args, "--cap-add", c)
	}
	for _, h := range s.AddHost {
		args = append(args, "--add-host", h)
	}
	args = append(args, s.Image)
	return append(args, s.Command...)
}

// Run creates and starts a container from spec.
func (r *Runtime) Run(ctx context.Context, spec RunSpec) error {
	if spec.Name == "" || spec.Image == "" {
		return errors.New("run needs a name and an image")
	}
	_, _, err := r.run(ctx, defaultCallTimeout, spec.RunArgs()...)
	return err
}

// Start starts an existing container.
func (r *Runtime) Start(ctx context.Context, name string) error {
	_, _, err := r.run(ctx, defaultCallTimeout, "start", name)
	return err
}

// Stop stops a container gracefully (SIGTERM, then SIGKILL after timeout).
func (r *Runtime) Stop(ctx context.Context, name string, timeout time.Duration) error {
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	_, _, err := r.run(ctx, timeout+defaultCallTimeout, "stop", "-t", fmt.Sprint(secs), name)
	return err
}

// Kill sends SIGKILL to a container.
func (r *Runtime) Kill(ctx context.Context, name string) error {
	_, _, err := r.run(ctx, defaultCallTimeout, "kill", "--signal", "KILL", name)
	return err
}

// Rm removes a container. With force it is stopped first.
func (r *Runtime) Rm(ctx context.Context, name string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, name)
	_, _, err := r.run(ctx, defaultCallTimeout, args...)
	return err
}

// Exists reports whether a container with this name exists (running or not).
func (r *Runtime) Exists(ctx context.Context, name string) (bool, error) {
	_, _, err := r.run(ctx, defaultCallTimeout, "container", "inspect", name)
	if err == nil {
		return true, nil
	}
	if IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// Running reports whether the container exists and is running.
func (r *Runtime) Running(ctx context.Context, name string) (bool, error) {
	out, err := r.Inspect(ctx, name, "{{.State.Running}}")
	if err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// Inspect returns one format-template field of a container.
func (r *Runtime) Inspect(ctx context.Context, name, format string) (string, error) {
	out, _, err := r.run(ctx, defaultCallTimeout, "inspect", "--format", format, name)
	return strings.TrimSpace(out), err
}

// Logs returns the stdout and stderr of a container, stdout first.
func (r *Runtime) Logs(ctx context.Context, name string) (string, error) {
	stdout, stderr, err := r.run(ctx, defaultCallTimeout, "logs", name)
	if err != nil {
		return "", err
	}
	return stdout + "\n" + stderr, nil
}

// Exec runs a command inside a running container and returns its stdout. A
// non-zero exit is an error whose message includes the command's stderr.
func (r *Runtime) Exec(ctx context.Context, name string, args ...string) (string, error) {
	full := append([]string{"exec", name}, args...)
	out, _, err := r.run(ctx, defaultCallTimeout, full...)
	return out, err
}

// ListContainers returns the names of all containers, running or not.
func (r *Runtime) ListContainers(ctx context.Context) ([]string, error) {
	return r.listNames(ctx, "ps", "--all", "--format", "{{.Names}}")
}

// ListVolumes returns the names of all volumes.
func (r *Runtime) ListVolumes(ctx context.Context) ([]string, error) {
	return r.listNames(ctx, "volume", "ls", "--format", "{{.Name}}")
}

// ListNetworks returns the names of all networks.
func (r *Runtime) ListNetworks(ctx context.Context) ([]string, error) {
	return r.listNames(ctx, "network", "ls", "--format", "{{.Name}}")
}

func (r *Runtime) listNames(ctx context.Context, args ...string) ([]string, error) {
	out, _, err := r.run(ctx, defaultCallTimeout, args...)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// NetworkCreate creates a bridge network. Creating an existing network is
// not an error.
func (r *Runtime) NetworkCreate(ctx context.Context, name string) error {
	_, _, err := r.run(ctx, defaultCallTimeout, "network", "create", name)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return err
}

// NetworkRemove removes a network.
func (r *Runtime) NetworkRemove(ctx context.Context, name string) error {
	_, _, err := r.run(ctx, defaultCallTimeout, "network", "rm", name)
	return err
}

// VolumeCreate creates a named volume. Creating an existing volume is not an
// error.
func (r *Runtime) VolumeCreate(ctx context.Context, name string) error {
	_, _, err := r.run(ctx, defaultCallTimeout, "volume", "create", name)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return err
}

// VolumeRemove removes a named volume.
func (r *Runtime) VolumeRemove(ctx context.Context, name string) error {
	_, _, err := r.run(ctx, defaultCallTimeout, "volume", "rm", name)
	return err
}

// NetworkIPs returns the IP address of the container on each network it is
// attached to, keyed by network name. Containers that share another
// container's network namespace have no entries.
func (r *Runtime) NetworkIPs(ctx context.Context, name string) (map[string]string, error) {
	raw, err := r.Inspect(ctx, name, "{{json .NetworkSettings.Networks}}")
	if err != nil {
		return nil, err
	}
	return parseNetworkIPs(raw)
}

// parseNetworkIPs decodes the JSON of .NetworkSettings.Networks. Both podman
// and docker answer with a map from network name to an object with an
// IPAddress field; an empty address is dropped.
func parseNetworkIPs(raw string) (map[string]string, error) {
	var nets map[string]struct {
		IPAddress string `json:"IPAddress"`
	}
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "null" {
		return map[string]string{}, nil
	}
	if err := json.Unmarshal([]byte(raw), &nets); err != nil {
		return nil, fmt.Errorf("decode network settings: %w", err)
	}
	out := make(map[string]string, len(nets))
	for name, n := range nets {
		if n.IPAddress != "" {
			out[name] = n.IPAddress
		}
	}
	return out, nil
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
