// Command spike is a throwaway prototype for AP-0.14 (ADR 0005/0006).
// Never copy code from here into the product.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

type Config struct {
	Name          string // also the TLS identity (cert CN)
	DataDir       string
	Listen        string // cluster TLS port (raft + h2)
	Advertise     string // stable DNS address, used as raft ServerAddress
	AdminAddr     string // plain HTTP for the scenario script
	CertDir       string
	Join          string
	RaftTimeout   time.Duration
	TrailingLogs  uint64
	SnapThreshold uint64
	SnapInterval  time.Duration
	HBInterval    time.Duration
	AEInterval    time.Duration
	CatchUpLag    uint64
	JoinHold      time.Duration // spike only: keep the membership mutex longer to force overlapping joins
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v, err := strconv.Atoi(env(k, strconv.Itoa(def)))
	if err != nil {
		slog.Error("invalid int env", "key", k, "err", err)
		os.Exit(2)
	}
	return v
}

func loadConfig() Config {
	return Config{
		Name:          env("NODE_NAME", "n1"),
		DataDir:       env("DATA_DIR", "/data"),
		Listen:        env("CLUSTER_LISTEN", ":9443"),
		Advertise:     env("CLUSTER_ADVERTISE", "raft-n1:9443"),
		AdminAddr:     env("ADMIN_LISTEN", ":8080"),
		CertDir:       env("CERT_DIR", "/certs"),
		Join:          os.Getenv("CLUSTER_JOIN"),
		RaftTimeout:   time.Duration(envInt("RAFT_TIMEOUT_MS", 1000)) * time.Millisecond,
		TrailingLogs:  uint64(envInt("RAFT_TRAILING_LOGS", 20000)),
		SnapThreshold: uint64(envInt("RAFT_SNAPSHOT_THRESHOLD", 8192)),
		SnapInterval:  time.Duration(envInt("RAFT_SNAPSHOT_INTERVAL_S", 120)) * time.Second,
		HBInterval:    time.Duration(envInt("HB_INTERVAL_MS", 100)) * time.Millisecond,
		AEInterval:    time.Duration(envInt("AE_INTERVAL_MS", 2000)) * time.Millisecond,
		CatchUpLag:    uint64(envInt("CATCHUP_LAG", 100)),
		JoinHold:      time.Duration(envInt("JOIN_HOLD_MS", 0)) * time.Millisecond,
	}
}

type Node struct {
	cfg    Config
	nodeID string
	raft   *raft.Raft
	fsm    *FSM
	hb     *hbStore
	repl   *replicator
	mux    *mux
	client *http.Client
	logs   *raftboltdb.BoltStore
	snaps  *raft.FileSnapshotStore

	memberMu sync.Mutex // serialises multi-step membership workflows (FA-CLU-04)

	startedAt time.Time
	bootInfo  map[string]any
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) > 1 && os.Args[1] == "pki" {
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: spike pki <dir> <name,name,...>")
			os.Exit(2)
		}
		if err := genPKI(os.Args[2], strings.Split(os.Args[3], ",")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 3 && os.Args[1] == "vacuumbench" {
		rows, _ := strconv.Atoi(os.Args[3])
		if err := vacuumBench(os.Args[2], rows); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	cfg := loadConfig()
	n, err := start(cfg)
	if err != nil {
		slog.Error("start failed", "err", err)
		os.Exit(1)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	n.shutdown()
}

func loadOrCreateNodeID(dir string) (string, bool, error) {
	p := filepath.Join(dir, "node_id")
	if b, err := os.ReadFile(p); err == nil {
		return strings.TrimSpace(string(b)), false, nil
	}
	id := uuid.NewString()
	return id, true, os.WriteFile(p, []byte(id+"\n"), 0o600)
}

func start(cfg Config) (*Node, error) {
	n := &Node{cfg: cfg, startedAt: time.Now(), bootInfo: map[string]any{}}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	id, fresh, err := loadOrCreateNodeID(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	n.nodeID = id
	slog.SetDefault(slog.Default().With("node", cfg.Name))

	tlsm, err := loadTLS(cfg.CertDir, cfg.Name)
	if err != nil {
		return nil, err
	}
	if n.mux, err = newMux(cfg.Listen, cfg.Advertise, tlsm); err != nil {
		return nil, err
	}
	n.client = n.mux.httpClient()

	if n.fsm, err = newFSM(cfg.DataDir); err != nil {
		return nil, fmt.Errorf("fsm: %w", err)
	}
	if n.hb, err = newHBStore(cfg.DataDir, id); err != nil {
		return nil, fmt.Errorf("hb: %w", err)
	}

	hl := hclog.New(&hclog.LoggerOptions{Name: "raft", Level: hclog.Info, Output: os.Stderr})
	if n.logs, err = raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft.db")); err != nil {
		return nil, err
	}
	if n.snaps, err = raft.NewFileSnapshotStoreWithLogger(cfg.DataDir, 2, hl); err != nil {
		return nil, err
	}
	hasState, err := raft.HasExistingState(n.logs, n.logs, n.snaps)
	if err != nil {
		return nil, err
	}

	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(id)
	rc.Logger = hl
	rc.HeartbeatTimeout = cfg.RaftTimeout
	rc.ElectionTimeout = cfg.RaftTimeout
	rc.LeaderLeaseTimeout = cfg.RaftTimeout / 2
	rc.TrailingLogs = cfg.TrailingLogs
	rc.SnapshotThreshold = cfg.SnapThreshold
	rc.SnapshotInterval = cfg.SnapInterval
	// state.db is durable and carries its own applied index, so raft does not
	// need to restore the latest snapshot on start - unless state.db is behind it.
	var snapIdx uint64
	if metas, _ := n.snaps.List(); len(metas) > 0 {
		snapIdx = metas[0].Index
	}
	rc.NoSnapshotRestoreOnStart = n.fsm.AppliedIndex() >= snapIdx

	tr := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream: n.mux.raftLayer(), MaxPool: 3, Timeout: 10 * time.Second, Logger: hl,
	})
	if n.raft, err = raft.NewRaft(rc, n.fsm, n.logs, n.logs, n.snaps, tr); err != nil {
		return nil, err
	}

	n.bootInfo = map[string]any{
		"fresh_node_id": fresh, "has_existing_state": hasState, "cluster_join_env": cfg.Join,
		"fsm_applied_index_at_start": n.fsm.AppliedIndex(), "latest_snapshot_index": snapIdx,
		"no_snapshot_restore_on_start": rc.NoSnapshotRestoreOnStart,
	}
	switch {
	case hasState:
		n.bootInfo["decision"] = "resume (existing raft state; CLUSTER_JOIN and bootstrap ignored)"
	case cfg.Join == "":
		n.bootInfo["decision"] = "bootstrap single-node cluster"
		f := n.raft.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{
			ID: raft.ServerID(id), Address: raft.ServerAddress(cfg.Advertise), Suffrage: raft.Voter,
		}}})
		if err := f.Error(); err != nil {
			return nil, fmt.Errorf("bootstrap: %w", err)
		}
	default:
		n.bootInfo["decision"] = "join " + cfg.Join
		go n.joinLoop()
	}
	slog.Info("boot decision", "node_id", id, "incarnation", n.hb.inc, "info", n.bootInfo)

	n.observe()

	ctx := context.Background()
	n.repl = &replicator{
		st: n.hb, client: n.client, peers: n.peerAddrs,
		interval: cfg.HBInterval, aeInterval: cfg.AEInterval, pageSize: 5000,
	}
	go n.repl.run(ctx)

	go func() {
		srv := &http.Server{Handler: n.clusterHandler(), ReadHeaderTimeout: 5 * time.Second}
		if err := srv.Serve(n.mux.httpListener()); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("cluster http", "err", err)
		}
	}()
	go func() {
		srv := &http.Server{Addr: cfg.AdminAddr, Handler: n.adminHandler(), ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("admin http", "err", err)
		}
	}()
	return n, nil
}

// observe logs leader changes with wall-clock timestamps for the scenario analysis.
func (n *Node) observe() {
	ch := make(chan raft.Observation, 16)
	n.raft.RegisterObserver(raft.NewObserver(ch, false, func(o *raft.Observation) bool {
		switch o.Data.(type) {
		case raft.LeaderObservation, raft.RaftState:
			return true
		}
		return false
	}))
	go func() {
		for o := range ch {
			switch d := o.Data.(type) {
			case raft.LeaderObservation:
				slog.Info("observed leader", "leader_id", d.LeaderID, "leader_addr", d.LeaderAddr, "ts_ms", time.Now().UnixMilli())
			case raft.RaftState:
				slog.Info("observed state", "state", d.String(), "ts_ms", time.Now().UnixMilli())
			}
		}
	}()
}

func (n *Node) peerAddrs() map[string]string {
	out := map[string]string{}
	f := n.raft.GetConfiguration()
	if f.Error() != nil {
		return out
	}
	for _, s := range f.Configuration().Servers {
		if string(s.ID) != n.nodeID {
			out[string(s.ID)] = string(s.Address)
		}
	}
	return out
}

func (n *Node) shutdown() {
	start := time.Now()
	if n.raft.State() == raft.Leader {
		err := n.raft.LeadershipTransfer().Error()
		slog.Info("leadership transfer on shutdown", "err", err, "ms", time.Since(start).Milliseconds())
	}
	_ = n.raft.Shutdown().Error()
	n.fsm.close()
	slog.Info("shutdown complete", "ms", time.Since(start).Milliseconds())
}
