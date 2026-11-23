// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/backoff"
	"github.com/joinsider/pulsequorum/internal/clusterpki"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// validClientOptions returns options that NewClient accepts. The buffer and
// metrics store are backed by a temporary SQLite database.
func validClientOptions(t *testing.T) ClientOptions {
	t.Helper()
	ms, buf := newTestBuffer(t, "probe-unit")
	return ClientOptions{
		NodeID:  "probe-unit",
		Cert:    newTestNodeCert(t, "probe-unit", "t1"),
		Voters:  newTestVoterList(t, "voter-a:443"),
		Buffer:  buf,
		Metrics: ms,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func newUnitClient(t *testing.T, mutate func(*ClientOptions)) *Client {
	t.Helper()
	opts := validClientOptions(t)
	if mutate != nil {
		mutate(&opts)
	}
	c, err := NewClient(opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// selfSignedNoIdentity returns a key and a certificate whose leaf carries no
// identity URI, so pki.IdentityOf rejects it.
func selfSignedNoIdentity(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "no-identity"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return key, der
}

// fakeUploadStream implements the client side of UploadHeartbeats. Sends are
// recorded (or fail with sendErr); Recv returns queued responses and then
// recvErr, or blocks until the stream context ends when recvErr is nil and
// the queue is empty.
type fakeUploadStream struct {
	grpc.ClientStream
	ctx     context.Context
	sendErr error
	recvErr error

	mu    sync.Mutex
	sent  []*pb.UploadHeartbeatsRequest
	queue []*pb.UploadHeartbeatsResponse
}

func (s *fakeUploadStream) Send(req *pb.UploadHeartbeatsRequest) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, req)
	return nil
}

func (s *fakeUploadStream) Recv() (*pb.UploadHeartbeatsResponse, error) {
	s.mu.Lock()
	if len(s.queue) > 0 {
		resp := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		return resp, nil
	}
	s.mu.Unlock()
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

// fakeProbeServiceClient opens fakeUploadStreams and never acknowledges
// anything, so inflight slots stay occupied.
type fakeProbeServiceClient struct {
	pb.ProbeServiceClient
	stream *fakeUploadStream
}

func (c *fakeProbeServiceClient) UploadHeartbeats(ctx context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[pb.UploadHeartbeatsRequest, pb.UploadHeartbeatsResponse], error) {
	c.stream.ctx = ctx
	return c.stream, nil
}

// scriptedMetrics returns batches in order and then fails or returns empty
// batches. onCall runs before the n-th call (1-based) returns.
type scriptedMetrics struct {
	batches [][]store.Heartbeat
	err     error
	onCall  func(n int)

	mu    sync.Mutex
	calls int
}

func (m *scriptedMetrics) HeartbeatsAfter(_ context.Context, _, _ string, _, _ uint64, _ int) ([]store.Heartbeat, bool, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	if m.onCall != nil {
		m.onCall(n)
	}
	if m.err != nil {
		return nil, false, m.err
	}
	if n <= len(m.batches) {
		return m.batches[n-1], true, nil
	}
	return nil, false, nil
}

// ackFailStore fails SetWatermark once fail is set, which makes Buffer.RecordAck fail.
type ackFailStore struct {
	*sqlite.MetricsStore
	fail atomic.Bool
}

func (s *ackFailStore) SetWatermark(ctx context.Context, name string, value int64) error {
	if s.fail.Load() {
		return errors.New("disk full")
	}
	return s.MetricsStore.SetWatermark(ctx, name, value)
}

// countingCancel returns a cancel func and a pointer to its call count.
func countingCancel() (context.CancelFunc, *atomic.Int32) {
	var n atomic.Int32
	return func() { n.Add(1) }, &n
}

func TestNewClientValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*ClientOptions)
		wantErr string
	}{
		{name: "missing node id", mutate: func(o *ClientOptions) { o.NodeID = "" }, wantErr: "nodeID is required"},
		{name: "missing cert", mutate: func(o *ClientOptions) { o.Cert = nil }, wantErr: "cert is required"},
		{name: "missing voters", mutate: func(o *ClientOptions) { o.Voters = nil }, wantErr: "voters is required"},
		{name: "missing buffer", mutate: func(o *ClientOptions) { o.Buffer = nil }, wantErr: "buffer is required"},
		{name: "missing metrics", mutate: func(o *ClientOptions) { o.Metrics = nil }, wantErr: "metrics store is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := validClientOptions(t)
			tt.mutate(&opts)
			c, err := NewClient(opts)
			if err == nil {
				t.Fatalf("NewClient = %v, want error containing %q", c, tt.wantErr)
			}
			if c != nil {
				t.Errorf("NewClient returned client %v with error", c)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewClientDefaultsLogger(t *testing.T) {
	t.Parallel()

	opts := validClientOptions(t)
	opts.Log = nil
	c, err := NewClient(opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.log != slog.Default() {
		t.Errorf("client logger is not slog.Default()")
	}
}

func TestClusterIDFunc(t *testing.T) {
	t.Parallel()

	noIdentityKey, noIdentityDER := selfSignedNoIdentity(t)

	tests := []struct {
		name  string
		setup func(t *testing.T) *Client
		want  string
	}{
		{
			name: "valid identity certificate",
			setup: func(t *testing.T) *Client {
				return newUnitClient(t, nil)
			},
			want: "cluster-test",
		},
		{
			name: "no certificate installed",
			setup: func(t *testing.T) *Client {
				return newUnitClient(t, func(o *ClientOptions) {
					o.Cert = &clusterpki.NodeCert{DataDir: t.TempDir(), NodeID: "probe-unit"}
				})
			},
			want: "",
		},
		{
			name: "leaf without identity URI",
			setup: func(t *testing.T) *Client {
				nc := &clusterpki.NodeCert{DataDir: t.TempDir(), NodeID: "probe-unit"}
				// Install needs a chain of at least two certificates whose
				// leaf matches the key.
				if err := nc.Install(noIdentityKey, clusterpki.Issued{
					ChainDER: [][]byte{noIdentityDER, noIdentityDER},
				}); err != nil {
					t.Fatalf("Install: %v", err)
				}
				return newUnitClient(t, func(o *ClientOptions) { o.Cert = nc })
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := tt.setup(t)
			if got := c.sec.Verifier.ClusterIDFunc(); got != tt.want {
				t.Errorf("ClusterIDFunc() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAfterSessionAttemptCertRejectedEstablished(t *testing.T) {
	t.Parallel()

	rejected := status.Error(codes.PermissionDenied, "certificate revoked")
	tests := []struct {
		name          string
		numCandidates int
		wantErr       error
		wantRejected  int
		wantAttempts  int
	}{
		{name: "single voter exits", numCandidates: 1, wantErr: ErrCertRejected, wantRejected: 1, wantAttempts: 1},
		{name: "several voters reconnect", numCandidates: 2, wantErr: nil, wantRejected: 1, wantAttempts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newUnitClient(t, nil)
			bo := backoff.FullJitter(time.Millisecond, time.Millisecond, nil)
			st := runRoundState{}
			err := c.afterSessionAttempt(context.Background(), bo, &st, "voter-a:443", tt.numCandidates, true, rejected)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("afterSessionAttempt = %v, want %v", err, tt.wantErr)
			}
			if st.roundRejected != tt.wantRejected || st.roundAttempts != tt.wantAttempts {
				t.Errorf("round state = {rejected %d, attempts %d}, want {rejected %d, attempts %d}",
					st.roundRejected, st.roundAttempts, tt.wantRejected, tt.wantAttempts)
			}
		})
	}
}

func TestApplyAssignmentPolicyAndRemovals(t *testing.T) {
	t.Parallel()

	t.Run("policy follows SSRF flag of assigned monitors", func(t *testing.T) {
		t.Parallel()
		policy := NewPolicy(false, false)
		syncer := newFakeMonitorSync()
		c := newUnitClient(t, func(o *ClientOptions) {
			o.Policy = policy
			o.Sync = syncer
		})

		c.applyAssignment(&pb.SubscribeResponse{
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{Id: "m1", TenantId: "t1", Type: "http", SsrfProtection: true},
			},
		})
		_ = syncer.waitCall(t, time.Second)
		if !policy.remoteSSRF.Load() {
			t.Fatalf("policy remote SSRF = false after snapshot with SSRF monitor, want true")
		}

		c.applyAssignment(&pb.SubscribeResponse{RemovedMonitorIds: []string{"m1"}})
		if policy.remoteSSRF.Load() {
			t.Errorf("policy remote SSRF = true after the SSRF monitor was removed, want false")
		}
		if got := syncer.waitCall(t, time.Second); len(got) != 0 {
			t.Errorf("Sync after removal = %+v, want no monitors", got)
		}
	})

	t.Run("removed ids and foreign or empty upserts are dropped", func(t *testing.T) {
		t.Parallel()
		syncer := newFakeMonitorSync()
		c := newUnitClient(t, func(o *ClientOptions) { o.Sync = syncer })

		c.applyAssignment(&pb.SubscribeResponse{
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{Id: "m1", TenantId: "t1", Type: "http"},
				{Id: "m2", TenantId: "t1", Type: "http"},
			},
		})
		_ = syncer.waitCall(t, time.Second)

		c.applyAssignment(&pb.SubscribeResponse{
			RemovedMonitorIds: []string{"m1"},
			Upserts: []*pb.ProbeMonitor{
				{Id: "m3", TenantId: "t2", Type: "http"}, // other tenant, discarded
				{Id: "", TenantId: "t1", Type: "http"},   // no id, discarded
				{Id: "m4", TenantId: "t1", Type: "http"},
			},
		})
		got := syncer.waitCall(t, time.Second)
		var ids []string
		for _, m := range got {
			ids = append(ids, m.ID)
		}
		if want := []string{"m2", "m4"}; !slices.Equal(ids, want) {
			t.Errorf("monitor IDs after update = %v, want %v", ids, want)
		}
	})
}

func TestRunUploadLoopStopsOnMetricsError(t *testing.T) {
	t.Parallel()

	metrics := &scriptedMetrics{err: errors.New("database locked")}
	c := newUnitClient(t, func(o *ClientOptions) {
		o.Metrics = metrics
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeProbeServiceClient{stream: &fakeUploadStream{}}

	c.runUploadLoop(ctx, client, 0, cancel)

	if ctx.Err() == nil {
		t.Fatalf("runUploadLoop returned without cancelling the session")
	}
	if metrics.calls != 1 {
		t.Errorf("HeartbeatsAfter calls = %d, want 1", metrics.calls)
	}
}

func TestRunUploadLoopStopsWhenSlotNeverFrees(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The window is one batch and the voter never acknowledges, so the second
	// batch waits for a slot. Cancelling the session while it waits must end the loop.
	metrics := &scriptedMetrics{
		batches: [][]store.Heartbeat{{{Seq: 1}}, {{Seq: 2}}},
		onCall: func(n int) {
			if n == 2 {
				cancel()
			}
		},
	}
	c := newUnitClient(t, func(o *ClientOptions) {
		o.Metrics = metrics
		o.MaxUploadWindow = 1
	})
	client := &fakeProbeServiceClient{stream: &fakeUploadStream{}}

	c.runUploadLoop(ctx, client, 0, cancel)

	if metrics.calls != 2 {
		t.Errorf("HeartbeatsAfter calls = %d, want 2", metrics.calls)
	}
	if n := len(client.stream.sent); n != 1 {
		t.Errorf("batches sent = %d, want 1 (second batch must wait for a slot)", n)
	}
}

func TestAbortUploadOnErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cancelCtx  bool
		wantCancel int32
	}{
		{name: "live session is cancelled", cancelCtx: false, wantCancel: 1},
		{name: "already cancelled session is left alone", cancelCtx: true, wantCancel: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newUnitClient(t, nil)
			ctx, ctxCancel := context.WithCancel(context.Background())
			defer ctxCancel()
			if tt.cancelCtx {
				ctxCancel()
			}
			cancel, n := countingCancel()
			c.abortUploadOnErr(ctx, cancel, "stream failed", errors.New("boom"))
			if got := n.Load(); got != tt.wantCancel {
				t.Errorf("cancelSession calls = %d, want %d", got, tt.wantCancel)
			}
		})
	}
}

func TestCheckUploadAlive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cancelCtx  bool
		recvErr    error
		wantAlive  bool
		wantCancel int32
	}{
		{name: "cancelled context", cancelCtx: true, wantAlive: false, wantCancel: 0},
		{name: "stream ended", recvErr: errors.New("eof"), wantAlive: false, wantCancel: 1},
		{name: "stream healthy", wantAlive: true, wantCancel: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newUnitClient(t, nil)
			ctx, ctxCancel := context.WithCancel(context.Background())
			defer ctxCancel()
			if tt.cancelCtx {
				ctxCancel()
			}
			recvErr := make(chan error, 1)
			if tt.recvErr != nil {
				recvErr <- tt.recvErr
			}
			cancel, n := countingCancel()
			if got := c.checkUploadAlive(ctx, recvErr, cancel); got != tt.wantAlive {
				t.Errorf("checkUploadAlive = %v, want %v", got, tt.wantAlive)
			}
			if got := n.Load(); got != tt.wantCancel {
				t.Errorf("cancelSession calls = %d, want %d", got, tt.wantCancel)
			}
		})
	}
}

func TestWaitForNewHeartbeats(t *testing.T) {
	t.Parallel()

	t.Run("cancelled context returns false", func(t *testing.T) {
		t.Parallel()
		c := newUnitClient(t, nil)
		ctx, ctxCancel := context.WithCancel(context.Background())
		ctxCancel()
		cancel, n := countingCancel()
		if c.waitForNewHeartbeats(ctx, make(chan error, 1), cancel) {
			t.Errorf("waitForNewHeartbeats = true, want false")
		}
		if n.Load() != 0 {
			t.Errorf("cancelSession called on a cancelled context")
		}
	})

	t.Run("stream error cancels session", func(t *testing.T) {
		t.Parallel()
		c := newUnitClient(t, nil)
		recvErr := make(chan error, 1)
		recvErr <- errors.New("stream reset")
		cancel, n := countingCancel()
		if c.waitForNewHeartbeats(context.Background(), recvErr, cancel) {
			t.Errorf("waitForNewHeartbeats = true, want false")
		}
		if n.Load() != 1 {
			t.Errorf("cancelSession calls = %d, want 1", n.Load())
		}
	})

	t.Run("notification wakes the loop", func(t *testing.T) {
		t.Parallel()
		c := newUnitClient(t, nil)
		c.NotifyNewHeartbeats(nil)
		cancel, n := countingCancel()
		if !c.waitForNewHeartbeats(context.Background(), make(chan error, 1), cancel) {
			t.Errorf("waitForNewHeartbeats = false, want true")
		}
		if n.Load() != 0 {
			t.Errorf("cancelSession calls = %d, want 0", n.Load())
		}
	})
}

func TestAcquireUploadSlot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fillWindow bool
		cancelCtx  bool
		recvErr    error
		wantOK     bool
		wantCancel int32
	}{
		{name: "free slot", wantOK: true},
		{name: "full window and cancelled context", fillWindow: true, cancelCtx: true, wantOK: false},
		{name: "full window and stream error", fillWindow: true, recvErr: errors.New("reset"), wantOK: false, wantCancel: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newUnitClient(t, nil)
			ctx, ctxCancel := context.WithCancel(context.Background())
			defer ctxCancel()
			if tt.cancelCtx {
				ctxCancel()
			}
			inflight := make(chan struct{}, 1)
			if tt.fillWindow {
				inflight <- struct{}{}
			}
			recvErr := make(chan error, 1)
			if tt.recvErr != nil {
				recvErr <- tt.recvErr
			}
			cancel, n := countingCancel()
			if got := c.acquireUploadSlot(ctx, inflight, recvErr, cancel); got != tt.wantOK {
				t.Errorf("acquireUploadSlot = %v, want %v", got, tt.wantOK)
			}
			if got := n.Load(); got != tt.wantCancel {
				t.Errorf("cancelSession calls = %d, want %d", got, tt.wantCancel)
			}
		})
	}
}

func TestSendUploadBatch(t *testing.T) {
	t.Parallel()

	hbs := []store.Heartbeat{{NodeID: "probe-unit", Seq: 8}, {NodeID: "probe-unit", Seq: 9}}

	t.Run("first batch carries lost-below marker", func(t *testing.T) {
		t.Parallel()
		c := newUnitClient(t, nil)
		stream := &fakeUploadStream{}
		lostSent := false
		var sentUpTo uint64 = 3
		cancel, n := countingCancel()

		if !c.sendUploadBatch(context.Background(), stream, hbs, 42, &lostSent, &sentUpTo, cancel) {
			t.Fatalf("sendUploadBatch = false, want true")
		}
		if len(stream.sent) != 1 {
			t.Fatalf("sent %d requests, want 1", len(stream.sent))
		}
		if got := stream.sent[0].GetLostBelow(); got != 42 {
			t.Errorf("LostBelow = %d, want 42", got)
		}
		if !lostSent {
			t.Errorf("lostSent = false after the marker was sent")
		}
		if sentUpTo != 9 {
			t.Errorf("sentUpTo = %d, want 9", sentUpTo)
		}
		if n.Load() != 0 {
			t.Errorf("cancelSession calls = %d, want 0", n.Load())
		}
	})

	t.Run("send failure cancels session", func(t *testing.T) {
		t.Parallel()
		c := newUnitClient(t, nil)
		stream := &fakeUploadStream{sendErr: errors.New("broken pipe")}
		lostSent := false
		var sentUpTo uint64 = 3
		cancel, n := countingCancel()

		if c.sendUploadBatch(context.Background(), stream, hbs, 42, &lostSent, &sentUpTo, cancel) {
			t.Fatalf("sendUploadBatch = true, want false")
		}
		if n.Load() != 1 {
			t.Errorf("cancelSession calls = %d, want 1", n.Load())
		}
		if sentUpTo != 3 {
			t.Errorf("sentUpTo = %d after failed send, want 3", sentUpTo)
		}
	})
}

func TestReceiveUploadAcks(t *testing.T) {
	t.Parallel()

	t.Run("persisted position is recorded", func(t *testing.T) {
		t.Parallel()
		ms, buf := newTestBuffer(t, "probe-unit")
		c := newUnitClient(t, func(o *ClientOptions) {
			o.Buffer = buf
			o.Metrics = ms
		})
		stream := &fakeUploadStream{queue: []*pb.UploadHeartbeatsResponse{
			{Persisted: &pb.StreamPosition{Seq: 5}},
		}, recvErr: io.EOF}
		recvErr := make(chan error, 1)

		// No in-flight slot is held, so the ack takes the default branch.
		c.receiveUploadAcks(context.Background(), stream, make(chan struct{}), recvErr)

		if err := <-recvErr; !errors.Is(err, io.EOF) {
			t.Errorf("stream error = %v, want io.EOF", err)
		}
		if got := buf.AckedSeq(); got != 5 {
			t.Errorf("AckedSeq = %d, want 5", got)
		}
	})

	t.Run("failed ack record is logged and the stream keeps reading", func(t *testing.T) {
		t.Parallel()
		ms := openTestMetricsStore(t)
		fs := &ackFailStore{MetricsStore: ms}
		buf, err := NewBuffer(context.Background(), BufferOptions{Store: fs, NodeID: "probe-unit"})
		if err != nil {
			t.Fatalf("NewBuffer: %v", err)
		}
		logs := &syncBuffer{}
		c := newUnitClient(t, func(o *ClientOptions) {
			o.Buffer = buf
			o.Metrics = ms
			o.Log = slog.New(slog.NewTextHandler(logs, nil))
		})
		fs.fail.Store(true)
		stream := &fakeUploadStream{queue: []*pb.UploadHeartbeatsResponse{
			{Persisted: &pb.StreamPosition{Seq: 7}},
		}, recvErr: io.EOF}
		recvErr := make(chan error, 1)

		c.receiveUploadAcks(context.Background(), stream, make(chan struct{}), recvErr)

		if err := <-recvErr; !errors.Is(err, io.EOF) {
			t.Errorf("stream error = %v, want io.EOF", err)
		}
		if got := string(logs.Bytes()); !strings.Contains(got, "recording heartbeat ack failed") {
			t.Errorf("log = %q, want it to report the failed ack", got)
		}
		if got := buf.AckedSeq(); got != 0 {
			t.Errorf("AckedSeq = %d after failed record, want 0", got)
		}
	})
}

func TestRenewCertFailureIsLogged(t *testing.T) {
	t.Parallel()

	logs := &syncBuffer{}
	cert := newTestNodeCert(t, "probe-unit", "t1")
	leaf := cert.Leaf()
	c := newUnitClient(t, func(o *ClientOptions) {
		o.Cert = cert
		o.Log = slog.New(slog.NewTextHandler(logs, nil))
		// Put the clock at the renewal point so the renewal is attempted.
		o.Clock = func() time.Time { return leaf.NotAfter }
	})

	// The voter does not implement the cluster service, so the renewal RPC fails.
	voter := newFakeProbeVoter(t)
	conn, err := newBufconnDialer(map[string]*fakeProbeVoter{"voter-a:443": voter})(context.Background(), "voter-a:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	c.renewCert(context.Background(), conn)

	got := string(logs.Bytes())
	if !strings.Contains(got, "renewing probe certificate failed") {
		t.Errorf("log = %q, want a renewal failure", got)
	}
	if strings.Contains(got, "probe certificate renewed") {
		t.Errorf("log = %q, must not report a successful renewal", got)
	}
}
