// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package harness

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

// Timing limits of container lifecycle steps.
const (
	healthTimeout  = 3 * time.Minute
	setupTokenWait = 60 * time.Second
	joinTimeout    = 5 * time.Minute
	joinTokenTTL   = 10 // minutes
	stopGrace      = 20 * time.Second
	secretsMount   = "/run/secrets/join-token"
	chownUID       = "65532:65532" // nonroot user of the distroless image
	displayNameAS  = "chaos harness"
)

var (
	setupTokenJSON = regexp.MustCompile(`"setup_token":"([^"\s]+)"`)
	setupTokenText = regexp.MustCompile(`setup_token=([^\s"]+)`)
)

// Cluster is one set of voters on one container network, with its own
// temp directory for join token files. A Cluster is used by one goroutine.
type Cluster struct {
	cfg   Config
	names Names
	rt    *Runtime
	log   *slog.Logger

	tmpDir string

	adminUser     string
	adminPassword string // kept in memory only, never logged

	voters  map[int]*voterState
	clients map[int]*ucapi.Client
	torn    bool
}

// voterState is what the harness knows about one voter slot.
type voterState struct {
	image   string // image the container was created from
	created bool   // a container has been created for this slot
	wiped   bool   // data was removed; only Join brings the node back
}

// StartOptions tunes Start.
type StartOptions struct {
	// Image replaces the voter image (rolling update). The container is
	// re-created from the same volume. Empty keeps the current image.
	Image string
}

// NewCluster validates cfg and prepares a temp directory (0700) for join
// token files. Call Teardown when done, also after failures.
func NewCluster(cfg Config, rt *Runtime, log *slog.Logger) (*Cluster, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cluster config: %w", err)
	}
	if rt == nil {
		return nil, errors.New("no container runtime")
	}
	if cfg.CallbackHost == "" {
		cfg.CallbackHost = DefaultCallbackHost(rt.Name)
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tmp, err := os.MkdirTemp("", cfg.Prefix+"-chaos-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	if err := os.Chmod(tmp, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit; it is private to the owner
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("chmod temp dir: %w", err)
	}
	return &Cluster{
		cfg:     cfg,
		names:   NamesFor(cfg.Prefix),
		rt:      rt,
		log:     log,
		tmpDir:  tmp,
		voters:  make(map[int]*voterState),
		clients: make(map[int]*ucapi.Client),
	}, nil
}

// Config returns the configuration of the cluster (with defaults applied).
func (c *Cluster) Config() Config { return c.cfg }

// Names returns the object name helper of the cluster.
func (c *Cluster) Names() Names { return c.names }

// CallbackHost returns the name containers use to reach the host.
func (c *Cluster) CallbackHost() string { return c.cfg.CallbackHost }

// Bootstrap prepares a fresh cluster: it removes stale objects of the prefix,
// creates the network, starts voter 1 alone (it bootstraps the cluster),
// reads the one-time setup token from its log and creates the admin with a
// random password. The password stays in memory.
func (c *Cluster) Bootstrap(ctx context.Context, adminUser string) error {
	if adminUser == "" {
		return errors.New("admin user name must not be empty")
	}
	if err := c.requireImages(ctx); err != nil {
		return err
	}
	if err := c.Purge(ctx); err != nil {
		return fmt.Errorf("remove stale objects: %w", err)
	}
	if err := c.rt.NetworkCreate(ctx, c.names.Network()); err != nil {
		return fmt.Errorf("create network %s: %w", c.names.Network(), err)
	}
	pw, err := randomPassword()
	if err != nil {
		return err
	}
	c.adminUser = adminUser
	c.adminPassword = pw
	c.clients = make(map[int]*ucapi.Client)

	if err := c.startVoter(ctx, 1, c.cfg.Image, nil); err != nil {
		return err
	}
	if err := c.WaitHealthy(ctx, 1, healthTimeout); err != nil {
		return err
	}
	token, err := c.readSetupToken(ctx, 1)
	if err != nil {
		return err
	}
	if err := c.Client(1).Setup(ctx, token, displayNameAS); err != nil {
		return fmt.Errorf("setup admin on %s: %w", c.names.Voter(1), err)
	}
	c.log.Info("admin created", "node", c.names.Voter(1), "user", adminUser)
	return nil
}

// requireImages checks that the voter and sidecar images exist locally.
func (c *Cluster) requireImages(ctx context.Context) error {
	for _, img := range []string{c.cfg.Image, c.cfg.NetemImage} {
		ok, err := c.rt.ImageExists(ctx, img)
		if err != nil {
			return fmt.Errorf("check image %s: %w", img, err)
		}
		if !ok {
			return fmt.Errorf("image %s is not present; build it or start the driver with -build", img)
		}
	}
	return nil
}

// Join adds voter i to the running cluster with a fresh join token created on
// the current leader. It waits until the node is healthy and listed as a
// voter. The token file exists only while the container starts.
func (c *Cluster) Join(ctx context.Context, i int) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	if running, err := c.rt.Running(ctx, c.names.Voter(i)); err != nil {
		return err
	} else if running {
		return fmt.Errorf("%s is already running", c.names.Voter(i))
	}
	leaderIdx, leaderAddr, err := c.leader(ctx)
	if err != nil {
		return fmt.Errorf("join %s: %w", c.names.Voter(i), err)
	}
	tok, err := c.Client(leaderIdx).CreateJoinToken(ctx, "voter", joinTokenTTL)
	if err != nil {
		return fmt.Errorf("create join token on %s: %w", c.names.Voter(leaderIdx), err)
	}
	tokenFile := filepath.Join(c.tmpDir, fmt.Sprintf("join-v%d.token", i))
	if err := writeReadableFile(tokenFile, tok.Token); err != nil {
		return err
	}
	tok.Token = ""
	defer func() { _ = os.Remove(tokenFile) }()

	if err := c.rt.VolumeCreate(ctx, c.names.Volume(i)); err != nil {
		return fmt.Errorf("create volume %s: %w", c.names.Volume(i), err)
	}
	if err := c.chownVolume(ctx, i); err != nil {
		return err
	}
	c.log.Info("joining voter", "node", c.names.Voter(i), "seed", leaderAddr)
	join := &joinSpec{Seed: leaderAddr, HostFile: tokenFile}
	if err := c.startVoter(ctx, i, c.cfg.Image, join); err != nil {
		return err
	}
	if err := c.WaitHealthy(ctx, i, healthTimeout); err != nil {
		return err
	}
	if err := c.waitMember(ctx, leaderIdx, i, joinTimeout); err != nil {
		return err
	}
	c.voters[i].wiped = false
	c.log.Info("voter joined", "node", c.names.Voter(i))
	return nil
}

// waitMember waits until the leader's member list shows voter i as voter.
func (c *Cluster) waitMember(ctx context.Context, viaIdx, i int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		list, err := c.Client(viaIdx).ListClusterNodes(ctx)
		if err == nil {
			for _, n := range list.Nodes {
				if n.Address == c.names.ClusterAddress(i) && n.Voter {
					return nil
				}
			}
			last = fmt.Errorf("%s is not a voter yet", c.names.Voter(i))
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not listed as voter within %s: %w", c.names.Voter(i), timeout, last)
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return err
		}
	}
}

// Start starts voter i again with its data. The container is re-created from
// the same volume with the given image (or the current one) and never joins.
// A voter whose data was removed must be brought back with Join.
func (c *Cluster) Start(ctx context.Context, i int, opts StartOptions) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	v := c.state(i)
	if v.wiped {
		return fmt.Errorf("%s has no data: bring it back with Join", c.names.Voter(i))
	}
	if !v.created {
		return fmt.Errorf("%s was never created: bring it up with Bootstrap or Join", c.names.Voter(i))
	}
	image := v.image
	if opts.Image != "" {
		image = opts.Image
	}
	running, err := c.rt.Running(ctx, c.names.Voter(i))
	if err != nil {
		return err
	}
	if running && image == v.image {
		return c.WaitHealthy(ctx, i, healthTimeout)
	}
	// The container is always re-created instead of started again: a join
	// container still has its token mount, whose file is gone after the join.
	if err := c.removeContainers(ctx, i); err != nil {
		return err
	}
	if err := c.startVoter(ctx, i, image, nil); err != nil {
		return err
	}
	return c.WaitHealthy(ctx, i, healthTimeout)
}

// Stop stops voter i gracefully (SIGTERM, then SIGKILL after the grace period).
func (c *Cluster) Stop(ctx context.Context, i int) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	return c.rt.Stop(ctx, c.names.Voter(i), stopGrace)
}

// Kill sends SIGKILL to voter i.
func (c *Cluster) Kill(ctx context.Context, i int) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	return c.rt.Kill(ctx, c.names.Voter(i))
}

// Remove deletes the container, the sidecar and the data volume of voter i.
// The node stays a member of the cluster until RemoveMember is called.
func (c *Cluster) Remove(ctx context.Context, i int) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	if err := c.removeContainers(ctx, i); err != nil {
		return err
	}
	if err := c.rt.VolumeRemove(ctx, c.names.Volume(i)); err != nil && !IsNotFound(err) {
		return fmt.Errorf("remove volume %s: %w", c.names.Volume(i), err)
	}
	c.state(i).wiped = true
	delete(c.clients, i)
	return nil
}

// WipeData removes the container and the data of voter i and leaves an empty
// volume. The node must be brought back with Join, which creates a new node
// identity; remove the old member with RemoveMember first.
func (c *Cluster) WipeData(ctx context.Context, i int) error {
	if err := c.Remove(ctx, i); err != nil {
		return err
	}
	if err := c.rt.VolumeCreate(ctx, c.names.Volume(i)); err != nil {
		return fmt.Errorf("create volume %s: %w", c.names.Volume(i), err)
	}
	return c.chownVolume(ctx, i)
}

// RemoveMember removes voter i from the Raft configuration through the
// current leader (admin API). The node must be stopped or removed first.
func (c *Cluster) RemoveMember(ctx context.Context, i int) (ucapi.NodeRemoval, error) {
	if err := c.checkIndex(i); err != nil {
		return ucapi.NodeRemoval{}, err
	}
	leaderIdx, _, err := c.leader(ctx)
	if err != nil {
		return ucapi.NodeRemoval{}, err
	}
	list, err := c.Client(leaderIdx).ListClusterNodes(ctx)
	if err != nil {
		return ucapi.NodeRemoval{}, err
	}
	for _, n := range list.Nodes {
		if n.Address == c.names.ClusterAddress(i) {
			return c.Client(leaderIdx).RemoveClusterNode(ctx, n.ID)
		}
	}
	return ucapi.NodeRemoval{}, fmt.Errorf("%s is not a member", c.names.Voter(i))
}

// Restart stops voter i and starts it again with its data. The sidecar is
// re-created as well.
func (c *Cluster) Restart(ctx context.Context, i int) error {
	if err := c.Stop(ctx, i); err != nil {
		return fmt.Errorf("restart %s: %w", c.names.Voter(i), err)
	}
	return c.Start(ctx, i, StartOptions{})
}

// Client returns the API client of voter i (it logs in on first use).
func (c *Cluster) Client(i int) *ucapi.Client {
	if cl, ok := c.clients[i]; ok {
		return cl
	}
	cl := ucapi.New(c.cfg.BaseURL(i), c.adminUser, c.adminPassword)
	c.clients[i] = cl
	return cl
}

// WaitHealthy polls GET /health of voter i until it answers or timeout.
func (c *Cluster) WaitHealthy(ctx context.Context, i int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		_, err := c.Client(i).Health(ctx)
		if err == nil {
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not healthy within %s: %w", c.names.Voter(i), timeout, last)
		}
		if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

// RunningVoters returns the indexes of voter containers that are running,
// ascending.
func (c *Cluster) RunningVoters(ctx context.Context) ([]int, error) {
	var out []int
	for i := 1; i <= c.cfg.Voters; i++ {
		running, err := c.rt.Running(ctx, c.names.Voter(i))
		if err != nil {
			return nil, err
		}
		if running {
			out = append(out, i)
		}
	}
	return out, nil
}

// Purge removes every container, volume and the network that belong to this
// prefix, including ones left by an earlier run. Sidecars go first.
func (c *Cluster) Purge(ctx context.Context) error {
	var errs []error
	containers, err := c.rt.ListContainers(ctx)
	if err != nil {
		return err
	}
	cre, vre, netName := stalePatterns(c.cfg.Prefix)
	matched := matchNames(containers, cre)
	sort.SliceStable(matched, func(a, b int) bool {
		return strings.HasSuffix(matched[a], "-netem") && !strings.HasSuffix(matched[b], "-netem")
	})
	for _, name := range matched {
		if err := c.rt.Rm(ctx, name, true); err != nil && !IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	volumes, err := c.rt.ListVolumes(ctx)
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, name := range matchNames(volumes, vre) {
			if err := c.rt.VolumeRemove(ctx, name); err != nil && !IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}
	networks, err := c.rt.ListNetworks(ctx)
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, name := range networks {
			if name == netName {
				if err := c.rt.NetworkRemove(ctx, name); err != nil && !IsNotFound(err) {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// Teardown removes everything the run created and the temp directory. With
// Config.Keep the objects and the temp directory are left in place. It is
// safe to call more than once.
func (c *Cluster) Teardown(ctx context.Context) error {
	if c.torn {
		return nil
	}
	c.torn = true
	if c.cfg.Keep {
		c.log.Warn("keeping containers, volumes, network and temp dir", "prefix", c.cfg.Prefix, "tmp", c.tmpDir)
		return nil
	}
	// Teardown must finish even if the caller's context is already done.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	err := c.Purge(cctx)
	if rmErr := os.RemoveAll(c.tmpDir); rmErr != nil {
		err = errors.Join(err, fmt.Errorf("remove temp dir: %w", rmErr))
	}
	return err
}

// joinSpec carries what a voter needs to join instead of bootstrapping.
type joinSpec struct {
	Seed     string // host:port of an existing voter
	HostFile string // host path of the token file, mounted read-only
}

// startVoter creates and starts the voter container of slot i, then its
// sidecar. Without join the node reuses the data of its volume (or bootstraps
// when the volume is empty, which Bootstrap relies on).
func (c *Cluster) startVoter(ctx context.Context, i int, image string, join *joinSpec) error {
	if err := c.rt.VolumeCreate(ctx, c.names.Volume(i)); err != nil {
		return fmt.Errorf("create volume %s: %w", c.names.Volume(i), err)
	}
	if err := c.chownVolume(ctx, i); err != nil {
		return err
	}
	if err := c.rt.Run(ctx, c.voterSpec(i, image, join)); err != nil {
		return fmt.Errorf("start %s: %w", c.names.Voter(i), err)
	}
	st := c.state(i)
	st.image = image
	st.created = true
	return c.startSidecar(ctx, i)
}

// voterSpec builds the container of voter i. It publishes the HTTP port on
// 127.0.0.1 only; the cluster port stays on the container network.
func (c *Cluster) voterSpec(i int, image string, join *joinSpec) RunSpec {
	env := map[string]string{
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
	spec := RunSpec{
		Name:    c.names.Voter(i),
		Image:   image,
		Network: c.names.Network(),
		Detach:  true,
		Env:     env,
		Publish: []string{fmt.Sprintf("127.0.0.1:%d:%d", c.cfg.HostPort(i), voterHTTPPort)},
		Volumes: []string{c.names.Volume(i) + ":/data"},
		AddHost: c.addHost(),
	}
	if c.cfg.AlertBundleWindow != "" {
		env["ALERT_BUNDLE_WINDOW"] = c.cfg.AlertBundleWindow
	}
	if len(c.cfg.SelfCheckTargets) > 0 {
		env["SELF_CHECK_TARGETS"] = strings.Join(c.cfg.SelfCheckTargets, ",")
	}
	if join != nil {
		env["CLUSTER_JOIN"] = join.Seed
		env["JOIN_TOKEN_FILE"] = secretsMount
		spec.Volumes = append(spec.Volumes, join.HostFile+":"+secretsMount+":ro")
	}
	return spec
}

// addHost maps the host name for docker, whose containers need an explicit
// host gateway entry. Podman provides host.containers.internal itself.
func (c *Cluster) addHost() []string {
	if c.rt.Name == RuntimeDocker {
		return []string{"host.docker.internal:host-gateway"}
	}
	return nil
}

// startSidecar (re)creates the network fault sidecar of voter i. It must be
// re-created whenever the voter container is re-created, because the sidecar
// shares the voter's network namespace.
func (c *Cluster) startSidecar(ctx context.Context, i int) error {
	if err := c.rt.Rm(ctx, c.names.Sidecar(i), true); err != nil && !IsNotFound(err) {
		return fmt.Errorf("remove old sidecar %s: %w", c.names.Sidecar(i), err)
	}
	spec := RunSpec{
		Name:             c.names.Sidecar(i),
		Image:            c.cfg.NetemImage,
		NetworkContainer: c.names.Voter(i),
		Detach:           true,
		CapAdd:           []string{"NET_ADMIN"},
		Command:          []string{"sleep", "infinity"},
	}
	if err := c.rt.Run(ctx, spec); err != nil {
		return fmt.Errorf("start sidecar %s: %w", c.names.Sidecar(i), err)
	}
	return nil
}

// removeContainers removes the voter container and its sidecar, keeping the
// volume.
func (c *Cluster) removeContainers(ctx context.Context, i int) error {
	for _, name := range []string{c.names.Sidecar(i), c.names.Voter(i)} {
		if err := c.rt.Rm(ctx, name, true); err != nil && !IsNotFound(err) {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	delete(c.clients, i)
	return nil
}

// chownVolume makes the data volume of voter i writable for the nonroot user
// of the image. A fresh named volume belongs to root; rootless podman maps
// that to the host user, so the chown runs as root inside the container.
func (c *Cluster) chownVolume(ctx context.Context, i int) error {
	name := c.cfg.Prefix + "-chown"
	if err := c.rt.Rm(ctx, name, true); err != nil && !IsNotFound(err) {
		return err
	}
	spec := RunSpec{
		Name:    name,
		Image:   c.cfg.NetemImage,
		User:    "0:0",
		Volumes: []string{c.names.Volume(i) + ":/data"},
		Rm:      true,
		Command: []string{"chown", chownUID, "/data"},
	}
	if err := c.rt.Run(ctx, spec); err != nil {
		return fmt.Errorf("chown volume %s: %w", c.names.Volume(i), err)
	}
	return nil
}

// readSetupToken waits for the node to log its one-time setup token.
func (c *Cluster) readSetupToken(ctx context.Context, i int) (string, error) {
	deadline := time.Now().Add(setupTokenWait)
	for {
		logs, err := c.rt.Logs(ctx, c.names.Voter(i))
		if err != nil {
			return "", fmt.Errorf("read logs of %s: %w", c.names.Voter(i), err)
		}
		if tok, ok := setupTokenFromLogs(logs); ok {
			return tok, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s logged no setup token within %s", c.names.Voter(i), setupTokenWait)
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return "", err
		}
	}
}

// setupTokenFromLogs extracts the one-time setup token from a JSON or text
// log. The token is a secret; callers must not print it.
func setupTokenFromLogs(logs string) (string, bool) {
	if m := setupTokenJSON.FindStringSubmatch(logs); m != nil {
		return m[1], true
	}
	if m := setupTokenText.FindStringSubmatch(logs); m != nil {
		return m[1], true
	}
	return "", false
}

// leader returns the index and advertised address of the current leader,
// asked from the running voters.
func (c *Cluster) leader(ctx context.Context) (int, string, error) {
	running, err := c.RunningVoters(ctx)
	if err != nil {
		return 0, "", err
	}
	if len(running) == 0 {
		return 0, "", errors.New("no running voter")
	}
	leaderID, err := c.LeaderOf(ctx, running)
	if err != nil {
		return 0, "", err
	}
	for _, j := range running {
		list, err := c.Client(j).ListClusterNodes(ctx)
		if err != nil {
			continue
		}
		for _, n := range list.Nodes {
			if n.ID != leaderID {
				continue
			}
			for k := 1; k <= c.cfg.Voters; k++ {
				if n.Address == c.names.ClusterAddress(k) {
					return k, n.Address, nil
				}
			}
			return 0, "", fmt.Errorf("leader address %q is not a voter of this run", n.Address)
		}
	}
	return 0, "", errors.New("leader not found in the member lists")
}

// state returns the state of slot i, creating it on first use.
func (c *Cluster) state(i int) *voterState {
	st, ok := c.voters[i]
	if !ok {
		st = &voterState{}
		c.voters[i] = st
	}
	return st
}

func (c *Cluster) checkIndex(i int) error {
	if i < 1 || i > c.cfg.Voters {
		return fmt.Errorf("voter index %d out of range 1..%d", i, c.cfg.Voters)
	}
	return nil
}

// randomPassword returns a random admin password of 32 characters.
func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeReadableFile writes secret text to path with mode 0644. The file is
// bind-mounted read-only into a container whose user is not the host user.
func writeReadableFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // G306: read by the container user; the directory is 0700
		return fmt.Errorf("write token file: %w", err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: see WriteFile above
		return fmt.Errorf("chmod token file: %w", err)
	}
	return nil
}

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
