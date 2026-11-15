// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// opsCallTimeout bounds one operation that runs a voter-image command.
const opsCallTimeout = 3 * time.Minute

// JoinAttempt is the outcome of a join that is expected to fail or to be
// observed in detail (see TryJoin).
type JoinAttempt struct {
	// Exited reports whether the joining container stopped within the wait.
	Exited bool
	// Logs is the combined log of the joining container.
	Logs string
}

// nodeEnv returns the environment of voter i. It mirrors voterSpec so that
// one-off admin commands read the same configuration as the node.
func (c *Cluster) nodeEnv(i int) map[string]string {
	return map[string]string{
		"NODE_NAME":         c.names.Voter(i),
		"NODE_ROLE":         "voter",
		"AUTH_MODE":         "local",
		"HTTP_ADDR":         fmt.Sprintf(":%d", voterHTTPPort),
		"CLUSTER_ADDR":      fmt.Sprintf(":%d", voterClusterPort),
		"CLUSTER_ADVERTISE": c.names.ClusterAddress(i),
		"SSRF_PROTECTION":   "off",
		"LOG_FORMAT":        "json",
		"DATA_DIR":          "/data",
	}
}

// OneShot runs "uptimecluster admin <args>" in a throwaway container on the
// data volume of voter i and returns its stdout. The voter must be stopped
// (raft-info, recover) or running (reset-password needs ExecStdin instead).
func (c *Cluster) OneShot(ctx context.Context, image string, i int, args ...string) (string, error) {
	if err := c.checkIndex(i); err != nil {
		return "", err
	}
	name := c.cfg.Prefix + "-admin"
	if err := c.rt.Rm(ctx, name, true); err != nil && !IsNotFound(err) {
		return "", err
	}
	spec := RunSpec{
		Name:    name,
		Image:   image,
		Env:     c.nodeEnv(i),
		Volumes: []string{c.names.Volume(i) + ":/data"},
		Rm:      true,
		Command: append([]string{"admin"}, args...),
	}
	ctxRun, cancel := context.WithTimeout(ctx, opsCallTimeout)
	defer cancel()
	return c.rt.Cmd(ctxRun, spec.RunArgs()...)
}

// CopyVolume copies the content of volume from into the new volume to, as
// root, with the fault sidecar image (it has a shell and cp). The caller
// removes the copy with RemoveObjects.
func (c *Cluster) CopyVolume(ctx context.Context, from, to string) error {
	if err := c.rt.VolumeCreate(ctx, to); err != nil {
		return fmt.Errorf("create volume %s: %w", to, err)
	}
	name := c.cfg.Prefix + "-copy"
	if err := c.rt.Rm(ctx, name, true); err != nil && !IsNotFound(err) {
		return err
	}
	spec := RunSpec{
		Name:    name,
		Image:   c.cfg.NetemImage,
		User:    "0:0",
		Volumes: []string{from + ":/src:ro", to + ":/dst"},
		Rm:      true,
		Command: []string{"sh", "-c", "cp -a /src/. /dst/"},
	}
	ctxRun, cancel := context.WithTimeout(ctx, opsCallTimeout)
	defer cancel()
	if err := c.rt.Run(ctxRun, spec); err != nil {
		return fmt.Errorf("copy volume %s to %s: %w", from, to, err)
	}
	return nil
}

// StartStale starts a voter container named name on the cluster network with
// the given volume and the node environment of slot i, without join and
// without a published port. It is used to bring a copy of an old data
// directory back. The caller removes the container with RemoveObjects.
func (c *Cluster) StartStale(ctx context.Context, name, volume string, i int) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	spec := c.voterSpec(i, c.cfg.Image, nil)
	spec.Name = name
	spec.Volumes = []string{volume + ":/data"}
	spec.Publish = nil
	spec.AddHost = nil
	if err := c.rt.Rm(ctx, name, true); err != nil && !IsNotFound(err) {
		return err
	}
	return c.rt.Run(ctx, spec)
}

// ContainerLogs returns the combined log of a container by name.
func (c *Cluster) ContainerLogs(ctx context.Context, name string) (string, error) {
	return c.rt.Logs(ctx, name)
}

// ContainerRunning reports whether a container by name is running.
func (c *Cluster) ContainerRunning(ctx context.Context, name string) (bool, error) {
	return c.rt.Running(ctx, name)
}

// RemoveObjects removes the named containers (forced) and volumes. Missing
// objects are not an error.
func (c *Cluster) RemoveObjects(ctx context.Context, containers, volumes []string) error {
	var errs []error
	for _, name := range containers {
		if err := c.rt.Rm(ctx, name, true); err != nil && !IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	for _, name := range volumes {
		if err := c.rt.VolumeRemove(ctx, name); err != nil && !IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// TryJoin starts voter i with a fresh join token, as Join does, but without
// the sidecar and without the health or membership wait. It waits up to wait
// for the container to stop and returns its log. A join that succeeds keeps
// the container running; then Exited is false and the caller decides what
// to do. The container is removed when it stopped. The data volume is used
// as it is: wipe it first to start from an empty directory.
func (c *Cluster) TryJoin(ctx context.Context, i int, wait time.Duration) (JoinAttempt, error) {
	if err := c.checkIndex(i); err != nil {
		return JoinAttempt{}, err
	}
	leaderIdx, leaderAddr, err := c.leader(ctx)
	if err != nil {
		return JoinAttempt{}, fmt.Errorf("join attempt of %s: %w", c.names.Voter(i), err)
	}
	tok, err := c.Client(leaderIdx).CreateJoinToken(ctx, "voter", joinTokenTTL)
	if err != nil {
		return JoinAttempt{}, fmt.Errorf("create join token on %s: %w", c.names.Voter(leaderIdx), err)
	}
	tokenFile := filepath.Join(c.tmpDir, fmt.Sprintf("attempt-v%d.token", i))
	if err := writeReadableFile(tokenFile, tok.Token); err != nil {
		return JoinAttempt{}, err
	}
	tok.Token = ""
	defer func() { _ = os.Remove(tokenFile) }()

	if err := c.rt.VolumeCreate(ctx, c.names.Volume(i)); err != nil {
		return JoinAttempt{}, fmt.Errorf("create volume %s: %w", c.names.Volume(i), err)
	}
	if err := c.chownVolume(ctx, i); err != nil {
		return JoinAttempt{}, err
	}
	spec := c.voterSpec(i, c.cfg.Image, &joinSpec{Seed: leaderAddr, HostFile: tokenFile})
	if err := c.rt.Rm(ctx, spec.Name, true); err != nil && !IsNotFound(err) {
		return JoinAttempt{}, err
	}
	if err := c.rt.Run(ctx, spec); err != nil {
		return JoinAttempt{}, fmt.Errorf("start joining %s: %w", c.names.Voter(i), err)
	}
	c.log.Info("join attempt started", "node", c.names.Voter(i), "seed", leaderAddr)

	deadline := time.Now().Add(wait)
	exited := false
	for {
		running, err := c.rt.Running(ctx, spec.Name)
		if err != nil {
			return JoinAttempt{}, err
		}
		if !running {
			exited = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return JoinAttempt{}, err
		}
	}
	logs, err := c.rt.Logs(ctx, spec.Name)
	if err != nil {
		return JoinAttempt{}, fmt.Errorf("read logs of %s: %w", spec.Name, err)
	}
	if exited {
		if err := c.rt.Rm(ctx, spec.Name, true); err != nil && !IsNotFound(err) {
			return JoinAttempt{}, err
		}
	}
	return JoinAttempt{Exited: exited, Logs: logs}, nil
}

// ExecStdin runs a command inside the running voter i and writes stdin to it
// (docker/podman exec -i). It returns stdout. The stdin text is not part of
// the error messages.
func (c *Cluster) ExecStdin(ctx context.Context, i int, stdin string, args ...string) (string, error) {
	if err := c.checkIndex(i); err != nil {
		return "", err
	}
	full := append([]string{"exec", "-i", c.names.Voter(i)}, args...)
	ctxRun, cancel := context.WithTimeout(ctx, opsCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctxRun, c.rt.Bin, full...) // #nosec G204 -- fixed runtime binary, arguments built by this package
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &CommandError{
			Command: c.rt.Name + " " + strings.Join(full[:3], " ") + " ...",
			Stderr:  truncate(strings.TrimSpace(stderr.String()), maxStderrInError),
			Err:     err,
		}
	}
	return stdout.String(), nil
}
