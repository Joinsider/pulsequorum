// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/backoff"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusternet"
	"github.com/joinsider/pulsequorum/internal/clusterpki"
	"github.com/joinsider/pulsequorum/internal/membership"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	probeProtocolVersion        = 1
	defaultSessionTimeout       = 15 * time.Second
	defaultConnectTimeout       = 5 * time.Second
	defaultMinBackoff           = 1 * time.Second
	defaultMaxBackoff           = 30 * time.Second
	defaultMaxUploadWindow      = 4
	defaultUploadBatchSize      = 500
	defaultRenewalCheckInterval = time.Hour
	renewCallTimeout            = 15 * time.Second
)

// ErrCertRejected is returned when the cluster permanently rejects the probe's
// certificate (revoked or foreign CA) and the probe must exit and re-join.
var ErrCertRejected = errors.New("probe certificate rejected by cluster; re-join required")

// MonitorSync receives the current set of monitors assigned to the probe.
type MonitorSync interface {
	Sync(monitors []store.Monitor)
}

// UploadMetricsStore reads buffered heartbeats for upload.
type UploadMetricsStore interface {
	HeartbeatsAfter(ctx context.Context, nodeID, incarnation string, afterSeq, untilSeq uint64, limit int) ([]store.Heartbeat, bool, error)
}

// ClientOptions configure a probe Client.
type ClientOptions struct {
	NodeID               string
	Name                 string
	Region               string
	Labels               map[string]string
	Capabilities         []string
	Version              string
	Cert                 *clusterpki.NodeCert
	Voters               *VoterList
	Buffer               *Buffer
	Metrics              UploadMetricsStore
	Sync                 MonitorSync
	Policy               *Policy
	SessionTimeout       time.Duration
	ConnectTimeout       time.Duration
	BackoffMin           time.Duration
	BackoffMax           time.Duration
	MaxUploadWindow      int
	UploadBatchSize      int
	RenewalCheckInterval time.Duration
	Dial                 func(ctx context.Context, addr string) (*grpc.ClientConn, error)
	RandIntN             func(int64) int64
	Clock                func() time.Time
	Log                  *slog.Logger
}

type assignedMonitor struct {
	proto *pb.ProbeMonitor
	store store.Monitor
}

// Client manages the probe's outbound mTLS connection to a voter, subscribes
// to monitor assignments, handles failover, renews certificates and CA bundles,
// and uploads buffered and live heartbeats.
type Client struct {
	opts ClientOptions
	log  *slog.Logger
	sec  clusternet.Security

	notifyCh chan struct{}
	ready    atomic.Bool

	assignMu sync.Mutex
	assigned map[string]assignedMonitor
	localRev uint64

	renewMu sync.Mutex
}

const msgUploadStreamEnded = "heartbeat upload stream ended"

// NewClient constructs a probe Client.
func NewClient(opts ClientOptions) (*Client, error) {
	if err := validateClientOptions(opts); err != nil {
		return nil, err
	}
	opts = applyClientDefaults(opts)
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	verifier := &pki.Verifier{
		ClusterIDFunc: func() string {
			if leaf := opts.Cert.Leaf(); leaf != nil {
				if id, err := pki.IdentityOf(leaf); err == nil {
					return id.ClusterID
				}
			}
			return ""
		},
		Roots: opts.Cert.Bundle,
		Now:   opts.Clock,
	}
	return &Client{
		opts:     opts,
		log:      log,
		sec:      clusternet.Security{Holder: &opts.Cert.Holder, Verifier: verifier},
		notifyCh: make(chan struct{}, 1),
		assigned: make(map[string]assignedMonitor),
	}, nil
}

func validateClientOptions(opts ClientOptions) error {
	switch {
	case opts.NodeID == "":
		return errors.New("probe client: nodeID is required")
	case opts.Cert == nil:
		return errors.New("probe client: cert is required")
	case opts.Voters == nil:
		return errors.New("probe client: voters is required")
	case opts.Buffer == nil:
		return errors.New("probe client: buffer is required")
	case opts.Metrics == nil:
		return errors.New("probe client: metrics store is required")
	}
	return nil
}

func applyClientDefaults(opts ClientOptions) ClientOptions {
	if opts.SessionTimeout <= 0 {
		opts.SessionTimeout = defaultSessionTimeout
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = defaultConnectTimeout
	}
	if opts.BackoffMin <= 0 {
		opts.BackoffMin = defaultMinBackoff
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = defaultMaxBackoff
	}
	if opts.MaxUploadWindow <= 0 {
		opts.MaxUploadWindow = defaultMaxUploadWindow
	}
	if opts.UploadBatchSize <= 0 {
		opts.UploadBatchSize = defaultUploadBatchSize
	}
	if opts.RenewalCheckInterval <= 0 {
		opts.RenewalCheckInterval = defaultRenewalCheckInterval
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return opts
}

// Ready reports whether the probe has an active voter session and received its
// initial assignment snapshot.
func (c *Client) Ready() error {
	if c.ready.Load() {
		return nil
	}
	return errors.New("probe is not connected to a voter")
}

// NotifyNewHeartbeats wakes the upload loop after new heartbeats are stored.
// It is non-blocking and safe to pass to HeartbeatWriter.SetOnStored.
func (c *Client) NotifyNewHeartbeats(_ []store.Heartbeat) {
	select {
	case c.notifyCh <- struct{}{}:
	default:
	}
}

func (c *Client) now() time.Time {
	return c.opts.Clock()
}

func (c *Client) dial(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	if c.opts.Dial != nil {
		return c.opts.Dial(ctx, addr)
	}
	return clusternet.DialProbe(addr, c.sec)
}

// Run maintains a probe session across voter failovers until ctx is cancelled
// or the cluster permanently rejects the probe certificate (ErrCertRejected).
func (c *Client) Run(ctx context.Context) error {
	bo := backoff.FullJitter(c.opts.BackoffMin, c.opts.BackoffMax, c.opts.RandIntN)
	var st runRoundState

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		candidates := c.opts.Voters.Candidates()
		if len(candidates) == 0 {
			if !backoff.Sleep(ctx, bo.Wait()) {
				return ctx.Err()
			}
			continue
		}

		addr := candidates[st.idx%len(candidates)]
		st.idx++

		established, err := c.runSession(ctx, addr)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.afterSessionAttempt(ctx, bo, &st, addr, len(candidates), established, err); err != nil {
			return err
		}
	}
}

type runRoundState struct {
	idx           int
	roundAttempts int
	roundRejected int
}

func (c *Client) afterSessionAttempt(ctx context.Context, bo *backoff.Backoff, st *runRoundState, addr string, numCandidates int, established bool, err error) error {
	if established {
		bo.Reset()
		st.roundAttempts = 0
		st.roundRejected = 0
		if isCertRejected(err) {
			st.roundRejected = 1
			st.roundAttempts = 1
			if numCandidates <= 1 {
				c.log.Error("probe certificate rejected by cluster; re-join required", "voter", addr, "err", err)
				return ErrCertRejected
			}
		}
		c.log.Info("probe session ended, reconnecting", "voter", addr, "err", err)
		return nil
	}

	st.roundAttempts++
	if isCertRejected(err) {
		st.roundRejected++
	}
	c.log.Warn("connecting to voter failed", "voter", addr, "err", err)

	if st.roundAttempts < numCandidates {
		return nil
	}
	if st.roundRejected > numCandidates/2 {
		c.log.Error("probe certificate rejected by a quorum of candidate voters; re-join required", "err", err)
		return ErrCertRejected
	}
	st.roundAttempts = 0
	st.roundRejected = 0
	delay := bo.Wait()
	c.log.Debug("all candidate voters failed, backing off", "delay", delay)
	if !backoff.Sleep(ctx, delay) {
		return ctx.Err()
	}
	return nil
}

func (c *Client) runSession(ctx context.Context, addr string) (bool, error) {
	dialCtx, dialCancel := context.WithTimeout(ctx, c.opts.ConnectTimeout)
	conn, err := c.dial(dialCtx, addr)
	dialCancel()
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	// Watchdog enforces ConnectTimeout until the first response, then SessionTimeout.
	var firstReceived atomic.Bool
	watchdog := time.NewTimer(c.opts.ConnectTimeout)
	defer watchdog.Stop()

	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-watchDone:
		case <-sessCtx.Done():
		case <-watchdog.C:
			if firstReceived.Load() {
				c.log.Warn("probe session tick timeout, failing over", "voter", addr, "timeout", c.opts.SessionTimeout)
			} else {
				c.log.Warn("probe session initial snapshot timeout, failing over", "voter", addr, "timeout", c.opts.ConnectTimeout)
			}
			sessCancel()
			_ = conn.Close()
		}
	}()

	req := c.buildSubscribeRequest(sessCtx)
	client := pb.NewProbeServiceClient(conn)
	stream, err := client.Subscribe(sessCtx, req)
	if err != nil {
		return false, err
	}

	var sessWg sync.WaitGroup
	defer func() {
		c.ready.Store(false)
		sessCancel()
		_ = conn.Close()
		sessWg.Wait()
	}()

	established := false
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return established, nil
			}
			return established, err
		}

		firstReceived.Store(true)
		resetTimer(watchdog, c.opts.SessionTimeout)

		if !established {
			established = true
			c.ready.Store(true)
			voterPos := resp.GetStreamPosition().GetSeq()
			sessWg.Go(func() { c.runUploadLoop(sessCtx, client, voterPos, sessCancel) })
			sessWg.Go(func() { c.runRenewalLoop(sessCtx, conn) })
		}

		c.handleSubscribeResponse(sessCtx, conn, resp)
	}
}

func (c *Client) buildSubscribeRequest(ctx context.Context) *pb.SubscribeRequest {
	req := &pb.SubscribeRequest{
		Capabilities:            slices.Clone(c.opts.Capabilities),
		Version:                 c.opts.Version,
		KnownRevision:           c.currentRevision(),
		ProtocolVersion:         probeProtocolVersion,
		Incarnation:             c.opts.Buffer.incarnation,
		Name:                    c.opts.Name,
		Region:                  c.opts.Region,
		Labels:                  maps.Clone(c.opts.Labels),
		WallClockUnixMs:         c.now().UnixMilli(),
		CertIssuerFingerprint:   c.opts.Cert.IssuerFingerprint(),
		TrustBundleFingerprints: c.opts.Cert.BundleFingerprints(),
	}
	if leaf := c.opts.Cert.Leaf(); leaf != nil {
		req.CertNotAfterMs = leaf.NotAfter.UnixMilli()
	}
	if st, err := c.opts.Buffer.Status(ctx); err == nil {
		req.Buffer = st
	}
	return req
}

func (c *Client) currentRevision() uint64 {
	c.assignMu.Lock()
	defer c.assignMu.Unlock()
	return c.localRev
}

func (c *Client) handleSubscribeResponse(ctx context.Context, conn *grpc.ClientConn, resp *pb.SubscribeResponse) {
	if voters := resp.GetVoters(); len(voters) > 0 {
		if err := c.opts.Voters.Update(voters); err != nil {
			c.log.Warn("updating learned voters failed", "err", err)
		}
	}
	c.applyAssignment(resp)
	c.maybeRenew(ctx, conn, resp)
}

func (c *Client) applyAssignment(resp *pb.SubscribeResponse) {
	if !resp.GetFullSnapshot() && len(resp.GetUpserts()) == 0 && len(resp.GetRemovedMonitorIds()) == 0 {
		return
	}

	probeTenant := c.probeTenantID()

	c.assignMu.Lock()
	defer c.assignMu.Unlock()

	if resp.GetFullSnapshot() {
		c.replaceAssignedLocked(probeTenant, resp.GetUpserts())
	} else {
		c.updateAssignedLocked(probeTenant, resp.GetRemovedMonitorIds(), resp.GetUpserts())
	}
	c.localRev++

	monitors, anySSRF := c.collectAssignedMonitorsLocked()
	if c.opts.Policy != nil {
		c.opts.Policy.SetRemote(anySSRF)
	}
	if c.opts.Sync != nil {
		c.opts.Sync.Sync(monitors)
	}
}

func (c *Client) replaceAssignedLocked(probeTenant string, upserts []*pb.ProbeMonitor) {
	next := make(map[string]assignedMonitor, len(upserts))
	for _, pm := range upserts {
		if !c.allowMonitor(probeTenant, pm) {
			continue
		}
		next[pm.GetId()] = c.nextAssignedMonitorLocked(pm)
	}
	c.assigned = next
}

func (c *Client) updateAssignedLocked(probeTenant string, removed []string, upserts []*pb.ProbeMonitor) {
	for _, id := range removed {
		delete(c.assigned, id)
	}
	for _, pm := range upserts {
		if !c.allowMonitor(probeTenant, pm) {
			continue
		}
		c.assigned[pm.GetId()] = c.nextAssignedMonitorLocked(pm)
	}
}

func (c *Client) nextAssignedMonitorLocked(pm *pb.ProbeMonitor) assignedMonitor {
	rev := uint64(1)
	if prev, ok := c.assigned[pm.GetId()]; ok {
		if probeMonitorEqual(prev.proto, pm) {
			rev = prev.store.Revision
		} else {
			rev = prev.store.Revision + 1
		}
	}
	return assignedMonitor{proto: pm, store: toStoreMonitor(pm, rev)}
}

func (c *Client) collectAssignedMonitorsLocked() ([]store.Monitor, bool) {
	monitors := make([]store.Monitor, 0, len(c.assigned))
	anySSRF := false
	for _, id := range slices.Sorted(maps.Keys(c.assigned)) {
		am := c.assigned[id]
		monitors = append(monitors, am.store)
		if am.proto.GetSsrfProtection() {
			anySSRF = true
		}
	}
	return monitors, anySSRF
}

// Defense-in-depth D6: a private probe discards monitors of other tenants.
func (c *Client) allowMonitor(probeTenant string, pm *pb.ProbeMonitor) bool {
	if pm == nil || pm.GetId() == "" {
		return false
	}
	if probeTenant != "" && pm.GetTenantId() != probeTenant {
		c.log.Error("discarded monitor belonging to another tenant",
			"probe_tenant", probeTenant, "monitor_tenant", pm.GetTenantId(), "monitor_id", pm.GetId())
		return false
	}
	return true
}

func (c *Client) probeTenantID() string {
	leaf := c.opts.Cert.Leaf()
	if leaf == nil {
		return ""
	}
	id, err := pki.IdentityOf(leaf)
	if err != nil {
		return ""
	}
	return id.TenantID
}

func (c *Client) maybeRenew(ctx context.Context, conn *grpc.ClientConn, resp *pb.SubscribeResponse) {
	needs := c.opts.Cert.NeedsRenewal(c.now())
	if !needs {
		if activeCA := resp.GetActiveCaFingerprint(); activeCA != "" && activeCA != c.opts.Cert.IssuerFingerprint() {
			needs = true
		}
	}
	if !needs {
		if remoteBundle := resp.GetTrustBundleFingerprints(); len(remoteBundle) > 0 {
			sorted := slices.Clone(remoteBundle)
			slices.Sort(sorted)
			sorted = slices.Compact(sorted)
			if !slices.Equal(sorted, c.opts.Cert.BundleFingerprints()) {
				needs = true
			}
		}
	}
	if !needs {
		return
	}
	c.renewCert(ctx, conn)
}

func (c *Client) runRenewalLoop(ctx context.Context, conn *grpc.ClientConn) {
	ticker := time.NewTicker(c.opts.RenewalCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.opts.Cert.NeedsRenewal(c.now()) {
				c.renewCert(ctx, conn)
			}
		}
	}
}

func (c *Client) renewCert(ctx context.Context, conn *grpc.ClientConn) {
	if !c.renewMu.TryLock() {
		return
	}
	defer c.renewMu.Unlock()

	issue := membership.RenewViaLeader(func() string { return "connected-voter" }, func(string) (pb.ClusterServiceClient, func(), error) {
		return pb.NewClusterServiceClient(conn), func() { /* connection is owned by runSession */ }, nil
	})
	callCtx, cancel := context.WithTimeout(ctx, renewCallTimeout)
	defer cancel()
	if err := c.opts.Cert.Renew(callCtx, c.now(), issue); err != nil && ctx.Err() == nil {
		c.log.Warn("renewing probe certificate failed", "err", err)
		return
	}
	c.log.Info("probe certificate renewed")
}

func (c *Client) runUploadLoop(ctx context.Context, client pb.ProbeServiceClient, voterPos uint64, cancelSession context.CancelFunc) {
	stream, err := client.UploadHeartbeats(ctx)
	if err != nil {
		c.abortUploadOnErr(ctx, cancelSession, "opening heartbeat upload stream failed", err)
		return
	}

	inflight := make(chan struct{}, c.opts.MaxUploadWindow)
	recvErr := make(chan error, 1)
	go c.receiveUploadAcks(ctx, stream, inflight, recvErr)

	sentUpTo, err := c.opts.Buffer.StartAfter(ctx, voterPos)
	if err != nil {
		c.abortUploadOnErr(ctx, cancelSession, "reading probe buffer start seq failed", err)
		return
	}

	lost := c.opts.Buffer.LostBelow()
	lostSent := lost == 0 || voterPos >= lost-1

	for {
		if !c.checkUploadAlive(ctx, recvErr, cancelSession) {
			return
		}
		hbs, _, err := c.opts.Metrics.HeartbeatsAfter(ctx, c.opts.NodeID, c.opts.Buffer.incarnation, sentUpTo, 0, c.opts.UploadBatchSize)
		if err != nil {
			c.abortUploadOnErr(ctx, cancelSession, "reading buffered heartbeats failed", err)
			return
		}
		if len(hbs) == 0 && lostSent {
			if !c.waitForNewHeartbeats(ctx, recvErr, cancelSession) {
				return
			}
			continue
		}
		if !c.acquireUploadSlot(ctx, inflight, recvErr, cancelSession) {
			return
		}
		if !c.sendUploadBatch(ctx, stream, hbs, lost, &lostSent, &sentUpTo, cancelSession) {
			return
		}
	}
}

func (c *Client) abortUploadOnErr(ctx context.Context, cancelSession context.CancelFunc, msg string, err error) {
	if ctx.Err() == nil {
		c.log.Warn(msg, "err", err)
		cancelSession()
	}
}

func (c *Client) receiveUploadAcks(ctx context.Context, stream pb.ProbeService_UploadHeartbeatsClient, inflight <-chan struct{}, recvErr chan<- error) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			recvErr <- err
			return
		}
		select {
		case <-inflight:
		default:
		}
		if pos := resp.GetPersisted(); pos != nil {
			if err := c.opts.Buffer.RecordAck(ctx, pos.GetSeq()); err != nil && ctx.Err() == nil {
				c.log.Warn("recording heartbeat ack failed", "seq", pos.GetSeq(), "err", err)
			}
		}
	}
}

func (c *Client) checkUploadAlive(ctx context.Context, recvErr <-chan error, cancelSession context.CancelFunc) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case err := <-recvErr:
		c.abortUploadOnErr(ctx, cancelSession, msgUploadStreamEnded, err)
		return false
	default:
		return true
	}
}

func (c *Client) waitForNewHeartbeats(ctx context.Context, recvErr <-chan error, cancelSession context.CancelFunc) bool {
	select {
	case <-ctx.Done():
		return false
	case err := <-recvErr:
		c.abortUploadOnErr(ctx, cancelSession, msgUploadStreamEnded, err)
		return false
	case <-c.notifyCh:
		return true
	}
}

func (c *Client) acquireUploadSlot(ctx context.Context, inflight chan<- struct{}, recvErr <-chan error, cancelSession context.CancelFunc) bool {
	select {
	case inflight <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case err := <-recvErr:
		c.abortUploadOnErr(ctx, cancelSession, msgUploadStreamEnded, err)
		return false
	}
}

func (c *Client) sendUploadBatch(ctx context.Context, stream pb.ProbeService_UploadHeartbeatsClient, hbs []store.Heartbeat, lost uint64, lostSent *bool, sentUpTo *uint64, cancelSession context.CancelFunc) bool {
	req := &pb.UploadHeartbeatsRequest{
		Heartbeats: heartbeatsToPB(hbs),
	}
	if !*lostSent {
		req.LostBelow = lost
		*lostSent = true
	}
	if err := stream.Send(req); err != nil {
		c.abortUploadOnErr(ctx, cancelSession, "sending heartbeats failed", err)
		return false
	}
	if len(hbs) > 0 {
		*sentUpTo = hbs[len(hbs)-1].Seq
	}
	return true
}

func toStoreMonitor(pm *pb.ProbeMonitor, rev uint64) store.Monitor {
	cfg := json.RawMessage(bytes.Clone(pm.GetConfigJson()))
	if len(cfg) == 0 {
		cfg = json.RawMessage("{}")
	}
	return store.Monitor{
		TenantID:        pm.GetTenantId(),
		ID:              pm.GetId(),
		Type:            pm.GetType(),
		Config:          cfg,
		IntervalSeconds: int(pm.GetIntervalSeconds()),
		TimeoutSeconds:  int(pm.GetTimeoutSeconds()),
		Revision:        rev,
	}
}

func probeMonitorEqual(a, b *pb.ProbeMonitor) bool {
	return a.GetId() == b.GetId() &&
		a.GetTenantId() == b.GetTenantId() &&
		a.GetType() == b.GetType() &&
		a.GetIntervalSeconds() == b.GetIntervalSeconds() &&
		a.GetTimeoutSeconds() == b.GetTimeoutSeconds() &&
		a.GetSsrfProtection() == b.GetSsrfProtection() &&
		bytes.Equal(a.GetConfigJson(), b.GetConfigJson())
}

func heartbeatsToPB(hbs []store.Heartbeat) []*pb.Heartbeat {
	out := make([]*pb.Heartbeat, len(hbs))
	for i, h := range hbs {
		details := h.Details
		if len(details) == 0 {
			details = []byte("{}")
		}
		out[i] = &pb.Heartbeat{
			NodeId:      h.NodeID,
			Incarnation: h.Incarnation,
			Seq:         h.Seq,
			TenantId:    h.TenantID,
			MonitorId:   h.MonitorID,
			TsMs:        h.TsMs,
			Status:      pb.CheckStatus(h.Status), //nolint:gosec // G115: status values 1..3
			LatencyMs:   uint32(min(max(h.LatencyMs, 0), math.MaxUint32)),
			ErrorClass:  cluster.ErrorClassToProto(h.ErrorClass),
			Message:     h.Message,
			DetailsJson: details,
		}
	}
	return out
}

func isCertRejected(err error) bool {
	if err == nil {
		return false
	}
	if status.Code(err) == codes.PermissionDenied {
		return true
	}
	if errors.Is(err, pki.ErrRevoked) || errors.Is(err, pki.ErrWrongCluster) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "revoked") ||
		strings.Contains(msg, "bad certificate") ||
		strings.Contains(msg, "unknown certificate authority") ||
		strings.Contains(msg, "certificate signed by unknown authority")
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
