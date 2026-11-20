// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package harness

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
)

func newTestCluster(t *testing.T, runtime string, cfg Config, handler func([]string) (string, error)) (*Cluster, *scripted) {
	t.Helper()
	sc := &scripted{handler: handler}
	rt := newRuntimeWithExec(runtime, sc.exec)
	cfg.Runtime = runtime
	c, err := NewCluster(cfg, rt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Teardown(context.Background()) })
	return c, sc
}

func TestNewClusterRejectsInvalidConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Prefix = "Bad_Prefix"
	if _, err := NewCluster(cfg, newRuntimeWithExec("podman", nil), nil); err == nil {
		t.Error("invalid prefix accepted")
	}
}

func TestNewClusterTempDirIsPrivate(t *testing.T) {
	c, _ := newTestCluster(t, "podman", DefaultConfig(), func([]string) (string, error) { return "", nil })
	info, err := os.Stat(c.tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("temp dir mode = %o, want 700", info.Mode().Perm())
	}
}

func TestVoterSpecPodmanJoin(t *testing.T) {
	c, _ := newTestCluster(t, "podman", DefaultConfig(), func([]string) (string, error) { return "", nil })
	spec := c.voterSpec(2, "pulsequorum:chaos", &joinSpec{Seed: "ucchaos-v1:9443", HostFile: "/tmp/t/join-v2.token"})
	args := strings.Join(spec.RunArgs(), " ")
	for _, want := range []string{
		"--name ucchaos-v2",
		"--network ucchaos-net",
		"-p 127.0.0.1:18102:8080",
		"-v ucchaos-v2-data:/data",
		"-v /tmp/t/join-v2.token:/run/secrets/join-token:ro",
		"-e CLUSTER_ADVERTISE=ucchaos-v2:9443",
		"-e CLUSTER_JOIN=ucchaos-v1:9443",
		"-e JOIN_TOKEN_FILE=/run/secrets/join-token",
		"-e SSRF_PROTECTION=off",
		"-e NODE_ROLE=voter",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("run args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "add-host") {
		t.Error("podman voter gets an add-host entry; podman does not need one")
	}
}

func TestVoterSpecDockerAddsHostGateway(t *testing.T) {
	c, _ := newTestCluster(t, "docker", DefaultConfig(), func([]string) (string, error) { return "", nil })
	spec := c.voterSpec(1, "pulsequorum:chaos", nil)
	args := strings.Join(spec.RunArgs(), " ")
	if !strings.Contains(args, "--add-host host.docker.internal:host-gateway") {
		t.Errorf("docker voter lacks host gateway:\n%s", args)
	}
	if strings.Contains(args, "CLUSTER_JOIN") || strings.Contains(args, "JOIN_TOKEN_FILE") {
		t.Error("bootstrap voter has join settings")
	}
}

func TestStartRefusesWipedAndNeverCreatedVoters(t *testing.T) {
	c, sc := newTestCluster(t, "podman", DefaultConfig(), func([]string) (string, error) { return "false", nil })
	ctx := context.Background()
	if err := c.Start(ctx, 2, StartOptions{}); err == nil || !strings.Contains(err.Error(), "never created") {
		t.Errorf("Start of an uncreated voter: %v", err)
	}
	c.state(2).created = true
	c.state(2).wiped = true
	if err := c.Start(ctx, 2, StartOptions{}); err == nil || !strings.Contains(err.Error(), "bring it back with Join") {
		t.Errorf("Start of a wiped voter: %v", err)
	}
	for _, call := range sc.calls {
		if strings.Contains(call, " run ") {
			t.Errorf("refused start still created a container: %s", call)
		}
	}
}

func TestJoinRejectsRunningAndBadIndex(t *testing.T) {
	c, _ := newTestCluster(t, "podman", DefaultConfig(), func(args []string) (string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "inspect --format {{.State.Running}}") {
			return "true", nil
		}
		return "", nil
	})
	ctx := context.Background()
	if err := c.Join(ctx, 2); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("Join of a running voter: %v", err)
	}
	if err := c.Join(ctx, 9); err == nil {
		t.Error("Join(9) accepted")
	}
}

func TestPurgeRemovesSidecarsFirstAndOnlyOwnPrefix(t *testing.T) {
	handler := func(args []string) (string, error) {
		switch strings.Join(args, " ") {
		case "ps --all --format {{.Names}}":
			return "ucchaos-v1\nucchaos-v1-netem\nucchaos-v2\nother-v1\nucchaos-v2-netem\n", nil
		case "volume ls --format {{.Name}}":
			return "ucchaos-v1-data\nother-v1-data\n", nil
		case "network ls --format {{.Name}}":
			return "ucchaos-net\nother-net\n", nil
		}
		return "", nil
	}
	c, sc := newTestCluster(t, "podman", DefaultConfig(), handler)
	if err := c.Purge(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rms, vols, nets []string
	for _, call := range sc.calls {
		switch {
		case strings.HasPrefix(call, "rm --force "):
			rms = append(rms, strings.TrimPrefix(call, "rm --force "))
		case strings.HasPrefix(call, "volume rm "):
			vols = append(vols, strings.TrimPrefix(call, "volume rm "))
		case strings.HasPrefix(call, "network rm "):
			nets = append(nets, strings.TrimPrefix(call, "network rm "))
		}
	}
	wantRms := []string{"ucchaos-v1-netem", "ucchaos-v2-netem", "ucchaos-v1", "ucchaos-v2"}
	if !reflect.DeepEqual(rms, wantRms) {
		t.Errorf("removed containers = %v, want %v", rms, wantRms)
	}
	if !reflect.DeepEqual(vols, []string{"ucchaos-v1-data"}) {
		t.Errorf("removed volumes = %v", vols)
	}
	if !reflect.DeepEqual(nets, []string{"ucchaos-net"}) {
		t.Errorf("removed networks = %v", nets)
	}
}

func TestTeardownKeepLeavesEverything(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Keep = true
	c, sc := newTestCluster(t, "podman", cfg, func([]string) (string, error) { return "", nil })
	if err := c.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sc.calls) != 0 {
		t.Errorf("Keep run still called the runtime: %v", sc.calls)
	}
	if _, err := os.Stat(c.tmpDir); err != nil {
		t.Errorf("Keep run removed the temp dir: %v", err)
	}
	_ = os.RemoveAll(c.tmpDir)
}

func TestTeardownIsIdempotentAndRemovesTempDir(t *testing.T) {
	sc := &scripted{handler: func([]string) (string, error) { return "", nil }}
	rt := newRuntimeWithExec("podman", sc.exec)
	c, err := NewCluster(DefaultConfig(), rt, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := c.tmpDir
	if err := c.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := len(sc.calls)
	if err := c.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sc.calls) != n {
		t.Error("second Teardown called the runtime again")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir still exists: %v", err)
	}
}

func TestWriteReadableFileMode(t *testing.T) {
	path := t.TempDir() + "/token"
	if err := writeReadableFile(path, "secret-value"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 644", info.Mode().Perm())
	}
}

func TestEnsureImagePolicies(t *testing.T) {
	present := func(_ []string) (string, error) { return "", nil }
	missing := func(args []string) (string, error) {
		if args[0] == "build" {
			return "", nil
		}
		return "", errors.New("Error: no such image")
	}
	spec := BuildSpec{Tag: "pulsequorum:chaos", Context: "."}
	tests := []struct {
		name    string
		handler func([]string) (string, error)
		policy  BuildPolicy
		built   bool
		wantErr bool
	}{
		{name: "auto keeps existing", handler: present, policy: BuildAuto},
		{name: "auto builds missing", handler: missing, policy: BuildAuto, built: true},
		{name: "always builds existing", handler: present, policy: BuildAlways, built: true},
		{name: "never with missing image", handler: missing, policy: BuildNever, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &scripted{handler: tt.handler}
			rt := newRuntimeWithExec("podman", sc.exec)
			built, err := EnsureImage(context.Background(), rt, spec, tt.policy, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if built != tt.built {
				t.Errorf("built = %v, want %v", built, tt.built)
			}
		})
	}
}

func TestParseBuildPolicy(t *testing.T) {
	if _, err := ParseBuildPolicy("sometimes"); err == nil {
		t.Error("unknown policy accepted")
	}
	if p, err := ParseBuildPolicy("auto"); err != nil || p != BuildAuto {
		t.Errorf("auto = %v, %v", p, err)
	}
}

func TestVoterSpecSelfCheckTargets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SelfCheckTargets = []string{"http://host.containers.internal:18952/healthz", "10.0.0.1:443"}
	c, _ := newTestCluster(t, "podman", cfg, func([]string) (string, error) { return "", nil })
	spec := strings.Join(c.voterSpec(1, "pulsequorum:chaos", nil).RunArgs(), " ")
	if !strings.Contains(spec, "-e SELF_CHECK_TARGETS=http://host.containers.internal:18952/healthz,10.0.0.1:443") {
		t.Errorf("run args lack SELF_CHECK_TARGETS: %s", spec)
	}
	plain, _ := newTestCluster(t, "podman", DefaultConfig(), func([]string) (string, error) { return "", nil })
	if strings.Contains(strings.Join(plain.voterSpec(1, "pulsequorum:chaos", nil).RunArgs(), " "), "SELF_CHECK_TARGETS") {
		t.Error("self-check env set without targets")
	}
}

func TestConfigRejectsBadSelfCheckTargets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SelfCheckTargets = []string{"a:1,b:2"}
	if err := cfg.Validate(); err == nil {
		t.Error("comma inside one target accepted")
	}
	cfg.SelfCheckTargets = []string{" "}
	if err := cfg.Validate(); err == nil {
		t.Error("blank target accepted")
	}
}

func TestVoterSpecAlertBundleWindow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AlertBundleWindow = "0s"
	c, _ := newTestCluster(t, "podman", cfg, func([]string) (string, error) { return "", nil })
	if !strings.Contains(strings.Join(c.voterSpec(1, "pulsequorum:chaos", nil).RunArgs(), " "), "-e ALERT_BUNDLE_WINDOW=0s") {
		t.Error("ALERT_BUNDLE_WINDOW not passed")
	}
	plain, _ := newTestCluster(t, "podman", DefaultConfig(), func([]string) (string, error) { return "", nil })
	if strings.Contains(strings.Join(plain.voterSpec(1, "pulsequorum:chaos", nil).RunArgs(), " "), "ALERT_BUNDLE_WINDOW") {
		t.Error("bundle window set without a value")
	}
	bad := DefaultConfig()
	bad.AlertBundleWindow = "soon"
	if err := bad.Validate(); err == nil {
		t.Error("invalid bundle window accepted")
	}
}
