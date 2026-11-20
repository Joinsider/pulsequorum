// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package harness

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// recorder is a fake runtime CLI. It records every call and answers with
// the configured stdout or stderr for the first matching prefix.
type recorder struct {
	calls   [][]string
	answers map[string]answer
}

type answer struct {
	stdout string
	stderr string
	err    error
}

func (r *recorder) exec(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	joined := strings.Join(args, " ")
	for prefix, a := range r.answers {
		if strings.HasPrefix(joined, prefix) {
			return []byte(a.stdout), []byte(a.stderr), a.err
		}
	}
	return nil, nil, nil
}

func (r *recorder) last() []string {
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

func TestResolveRuntime(t *testing.T) {
	found := func(names ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			for _, f := range names {
				if f == n {
					return "/bin/" + n, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	tests := []struct {
		name    string
		flag    string
		look    func(string) (string, error)
		want    string
		wantErr bool
	}{
		{name: "empty prefers podman", look: found("podman", "docker"), want: "podman"},
		{name: "empty falls back to docker", look: found("docker"), want: "docker"},
		{name: "empty and nothing installed", look: found(), wantErr: true},
		{name: "explicit docker", flag: "docker", look: found("docker", "podman"), want: "docker"},
		{name: "explicit podman missing", flag: "podman", look: found("docker"), wantErr: true},
		{name: "unknown runtime", flag: "nerdctl", look: found("podman", "docker"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveRuntime(tt.flag, tt.look)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("runtime = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunArgsArePodmanAndDockerCompatible(t *testing.T) {
	spec := RunSpec{
		Name:    "ucchaos-v2",
		Image:   "pulsequorum:chaos",
		Network: "ucchaos-net",
		Detach:  true,
		Env:     map[string]string{"NODE_NAME": "ucchaos-v2", "AUTH_MODE": "local", "CLUSTER_JOIN": "ucchaos-v1:9443"},
		Publish: []string{"127.0.0.1:18102:8080"},
		Volumes: []string{"ucchaos-v2-data:/data", "/tmp/x/join.token:/run/secrets/join-token:ro"},
		AddHost: []string{"host.docker.internal:host-gateway"},
	}
	want := []string{
		"run", "--pull=never", "--name", "ucchaos-v2", "--network", "ucchaos-net", "-d",
		"-e", "AUTH_MODE=local", "-e", "CLUSTER_JOIN=ucchaos-v1:9443", "-e", "NODE_NAME=ucchaos-v2",
		"-p", "127.0.0.1:18102:8080",
		"-v", "ucchaos-v2-data:/data", "-v", "/tmp/x/join.token:/run/secrets/join-token:ro",
		"--add-host", "host.docker.internal:host-gateway",
		"pulsequorum:chaos",
	}
	if got := spec.RunArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("RunArgs =\n %v\nwant\n %v", got, want)
	}
}

func TestRunArgsSidecarSharesNamespace(t *testing.T) {
	spec := RunSpec{
		Name:             "ucchaos-v1-netem",
		Image:            "uc-chaos-netem:latest",
		NetworkContainer: "ucchaos-v1",
		Detach:           true,
		CapAdd:           []string{"NET_ADMIN"},
		Command:          []string{"sleep", "infinity"},
	}
	got := strings.Join(spec.RunArgs(), " ")
	want := "run --pull=never --name ucchaos-v1-netem --network container:ucchaos-v1 -d --cap-add NET_ADMIN uc-chaos-netem:latest sleep infinity"
	if got != want {
		t.Errorf("RunArgs = %q\nwant %q", got, want)
	}
}

func TestRunArgsUserAndRm(t *testing.T) {
	spec := RunSpec{Name: "c", Image: "i", User: "0:0", Rm: true, Command: []string{"chown", "65532:65532", "/data"}}
	got := strings.Join(spec.RunArgs(), " ")
	if !strings.Contains(got, "--rm") || !strings.Contains(got, "--user 0:0") {
		t.Errorf("RunArgs = %q, want --rm and --user", got)
	}
}

func TestBuildArgsSortedBuildArgs(t *testing.T) {
	spec := BuildSpec{Tag: "pulsequorum:chaos", Context: ".", BuildArgs: map[string]string{"VERSION": "chaos", "COMMIT": "abc"}}
	want := []string{"build", "-t", "pulsequorum:chaos", "--build-arg", "COMMIT=abc", "--build-arg", "VERSION=chaos", "."}
	if got := spec.args(); !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestRuntimeCallsRecordedCommands(t *testing.T) {
	rec := &recorder{answers: map[string]answer{
		"container inspect ucchaos-v9": {stderr: "Error: no such container ucchaos-v9", err: errors.New("exit 125")},
		"volume rm ":                   {stderr: "Error: no such volume x", err: errors.New("exit 125")},
		"inspect --format {{.State.Running}} ucchaos-v1": {stdout: "true\n"},
	}}
	rt := newRuntimeWithExec("podman", rec.exec)
	ctx := context.Background()

	ok, err := rt.Exists(ctx, "ucchaos-v9")
	if err != nil || ok {
		t.Errorf("Exists(missing) = %v, %v; want false, nil", ok, err)
	}
	if err := rt.VolumeRemove(ctx, "x"); !IsNotFound(err) {
		t.Errorf("VolumeRemove of missing volume: err = %v, want not-found", err)
	}
	running, err := rt.Running(ctx, "ucchaos-v1")
	if err != nil || !running {
		t.Errorf("Running = %v, %v; want true", running, err)
	}
	if err := rt.Kill(ctx, "ucchaos-v1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.last(), " "); got != "kill --signal KILL ucchaos-v1" {
		t.Errorf("kill args = %q", got)
	}
}

func TestCommandErrorTruncatesStderr(t *testing.T) {
	long := strings.Repeat("e", 1000)
	rec := &recorder{answers: map[string]answer{"start ": {stderr: long, err: errors.New("exit 1")}}}
	rt := newRuntimeWithExec("docker", rec.exec)
	err := rt.Start(context.Background(), "c")
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want CommandError", err)
	}
	if len(ce.Stderr) > maxStderrInError+3 {
		t.Errorf("stderr not truncated: %d bytes", len(ce.Stderr))
	}
	if !strings.HasPrefix(ce.Error(), "docker start c: exit 1: ") {
		t.Errorf("error = %q", ce.Error())
	}
}

func TestStopUsesGraceTimeout(t *testing.T) {
	rec := &recorder{}
	rt := newRuntimeWithExec("podman", rec.exec)
	if err := rt.Stop(context.Background(), "c", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.last(), " "); got != "stop -t 20 c" {
		t.Errorf("stop args = %q", got)
	}
}

func TestParseNetworkIPs(t *testing.T) {
	raw := `{"ucchaos-net":{"IPAddress":"10.89.0.7","Gateway":"10.89.0.1"},"other":{"IPAddress":""}}`
	got, err := parseNetworkIPs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]string{"ucchaos-net": "10.89.0.7"}) {
		t.Errorf("ips = %v", got)
	}
	for _, empty := range []string{"", "null", "{}"} {
		got, err := parseNetworkIPs(empty)
		if err != nil || len(got) != 0 {
			t.Errorf("parseNetworkIPs(%q) = %v, %v", empty, got, err)
		}
	}
	if _, err := parseNetworkIPs("{not json"); err == nil {
		t.Error("broken JSON accepted")
	}
}

func TestListNamesSplitsLines(t *testing.T) {
	rec := &recorder{answers: map[string]answer{"ps --all": {stdout: "ucchaos-v1\nucchaos-v1-netem\n\n  ucchaos-v2 \n"}}}
	rt := newRuntimeWithExec("podman", rec.exec)
	names, err := rt.ListContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ucchaos-v1", "ucchaos-v1-netem", "ucchaos-v2"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestVolumeCreateIsIdempotent(t *testing.T) {
	rec := &recorder{answers: map[string]answer{
		"volume create ucchaos-v2-data": {stderr: "Error: volume with name ucchaos-v2-data already exists: volume already exists", err: errors.New("exit 125")},
	}}
	rt := newRuntimeWithExec("podman", rec.exec)
	if err := rt.VolumeCreate(context.Background(), "ucchaos-v2-data"); err != nil {
		t.Errorf("creating an existing volume failed: %v", err)
	}
	if err := rt.NetworkCreate(context.Background(), "ucchaos-net"); err != nil {
		t.Errorf("NetworkCreate of a new network answered with the fake: %v", err)
	}
}
