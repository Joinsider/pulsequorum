package main

// Heartbeat replication outside raft (ADR 0006): key (node_id, incarnation,
// seq), idempotent inserts, best-effort push per peer with a bounded buffer,
// plus pull-based anti-entropy (watermarks + FetchRange) periodically and
// right after a peer becomes reachable again.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type HB struct {
	NodeID string `json:"n"`
	Inc    string `json:"i"`
	Seq    int64  `json:"s"`
	TsMs   int64  `json:"t"`
}

type hbStore struct {
	w, r   *sql.DB
	nodeID string
	inc    string
	seq    atomic.Int64
}

const hbSchema = `
CREATE TABLE IF NOT EXISTS hb_meta(id INTEGER PRIMARY KEY CHECK(id=1), incarnation TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS hb(
  node_id TEXT NOT NULL, incarnation TEXT NOT NULL, seq INTEGER NOT NULL, ts_ms INTEGER NOT NULL,
  PRIMARY KEY(node_id, incarnation, seq)) WITHOUT ROWID;
`

// newHBStore keeps the incarnation inside hb.db itself: a fresh heartbeat
// database always means a fresh incarnation, so seq can never restart at 1
// under an incarnation that peers already know (PF-12).
func newHBStore(dir, nodeID string) (*hbStore, error) {
	path := filepath.Join(dir, "hb.db")
	w, err := openSQLite(path, 1)
	if err != nil {
		return nil, err
	}
	r, err := openSQLite(path, 4)
	if err != nil {
		return nil, err
	}
	if _, err := w.Exec(hbSchema); err != nil {
		return nil, fmt.Errorf("hb schema: %w", err)
	}
	s := &hbStore{w: w, r: r, nodeID: nodeID}
	err = w.QueryRow(`SELECT incarnation FROM hb_meta WHERE id=1`).Scan(&s.inc)
	if errors.Is(err, sql.ErrNoRows) {
		s.inc = uuid.NewString()[:8]
		_, err = w.Exec(`INSERT INTO hb_meta(id, incarnation) VALUES(1, ?)`, s.inc)
	}
	if err != nil {
		return nil, err
	}
	var maxSeq int64
	_ = w.QueryRow(`SELECT coalesce(max(seq),0) FROM hb WHERE node_id=? AND incarnation=?`, nodeID, s.inc).Scan(&maxSeq)
	s.seq.Store(maxSeq)
	return s, nil
}

func (s *hbStore) next() HB {
	return HB{NodeID: s.nodeID, Inc: s.inc, Seq: s.seq.Add(1), TsMs: time.Now().UnixMilli()}
}

// insert is idempotent (ON CONFLICT DO NOTHING) and returns the number of new rows.
func (s *hbStore) insert(batch []HB) (int64, error) {
	tx, err := s.w.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO hb(node_id,incarnation,seq,ts_ms) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	var added int64
	for _, h := range batch {
		res, err := stmt.Exec(h.NodeID, h.Inc, h.Seq, h.TsMs)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		added += n
	}
	return added, tx.Commit()
}

type Watermark struct {
	NodeID string `json:"node_id"`
	Inc    string `json:"incarnation"`
	Contig int64  `json:"contig"` // all seq in 1..contig are present
	Max    int64  `json:"max"`
	Count  int64  `json:"count"`
	SumSeq int64  `json:"sum_seq"` // cheap checksum
}

func wmKey(node, inc string) string { return node + "/" + inc }

// watermarks returns the *contiguous* high-water mark per stream. max(seq)
// alone is not enough: the push path can deliver seq 200 while 150..199 were
// dropped, and a max-based watermark would never fetch the gap.
func (s *hbStore) watermarks() (map[string]Watermark, error) {
	rows, err := s.r.Query(`SELECT node_id, incarnation, max(seq), count(*), sum(seq) FROM hb GROUP BY node_id, incarnation`)
	if err != nil {
		return nil, err
	}
	out := map[string]Watermark{}
	for rows.Next() {
		var w Watermark
		if err := rows.Scan(&w.NodeID, &w.Inc, &w.Max, &w.Count, &w.SumSeq); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out[wmKey(w.NodeID, w.Inc)] = w
	}
	_ = rows.Close()
	for k, w := range out {
		if w.Count == w.Max {
			w.Contig = w.Max
		} else {
			_ = s.r.QueryRow(`SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM hb WHERE node_id=?1 AND incarnation=?2 AND seq=1) THEN 0 ELSE
				(SELECT min(h.seq) FROM hb h WHERE h.node_id=?1 AND h.incarnation=?2 AND NOT EXISTS
				  (SELECT 1 FROM hb x WHERE x.node_id=h.node_id AND x.incarnation=h.incarnation AND x.seq=h.seq+1)) END`,
				w.NodeID, w.Inc).Scan(&w.Contig)
		}
		out[k] = w
	}
	return out, nil
}

func (s *hbStore) fetchRange(node, inc string, after int64, limit int) ([]HB, error) {
	rows, err := s.r.Query(`SELECT node_id, incarnation, seq, ts_ms FROM hb WHERE node_id=? AND incarnation=? AND seq>? ORDER BY seq LIMIT ?`,
		node, inc, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HB
	for rows.Next() {
		var h HB
		if err := rows.Scan(&h.NodeID, &h.Inc, &h.Seq, &h.TsMs); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- replicator ----

type peerStats struct {
	Addr        string `json:"addr"`
	Reachable   bool   `json:"reachable"`
	Pushed      int64  `json:"pushed"`
	Dropped     int64  `json:"dropped_buffer_full"`
	PushErrors  int64  `json:"push_errors"`
	AERuns      int64  `json:"ae_runs"`
	AEFetched   int64  `json:"ae_fetched_new_rows"`
	LastAEError string `json:"last_ae_error,omitempty"`
}

type peerOut struct {
	ch      chan HB
	trigger chan struct{}
	cancel  context.CancelFunc
	mu      sync.Mutex
	st      peerStats
}

type replicator struct {
	st         *hbStore
	client     *http.Client
	peers      func() map[string]string // node id -> address (from the raft configuration)
	interval   time.Duration
	aeInterval time.Duration
	pageSize   int

	mu  sync.Mutex
	out map[string]*peerOut
}

func (r *replicator) run(ctx context.Context) {
	tick := time.NewTicker(r.interval)
	peerSync := time.NewTicker(time.Second)
	defer tick.Stop()
	defer peerSync.Stop()
	r.syncPeers(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-peerSync.C:
			r.syncPeers(ctx)
		case <-tick.C:
			h := r.st.next()
			if _, err := r.st.insert([]HB{h}); err != nil {
				slog.Error("hb insert", "err", err)
			}
			r.mu.Lock()
			for _, p := range r.out {
				select {
				case p.ch <- h:
				default:
					// Bounded buffer full: never block the checks, AE will catch up.
					p.mu.Lock()
					p.st.Dropped++
					p.mu.Unlock()
				}
			}
			r.mu.Unlock()
		}
	}
}

func (r *replicator) syncPeers(ctx context.Context) {
	want := r.peers()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out == nil {
		r.out = map[string]*peerOut{}
	}
	for id, addr := range want {
		if p, ok := r.out[id]; ok && p.st.Addr == addr {
			continue
		} else if ok {
			p.cancel()
		}
		pctx, cancel := context.WithCancel(ctx)
		p := &peerOut{ch: make(chan HB, 256), trigger: make(chan struct{}, 1), cancel: cancel, st: peerStats{Addr: addr}}
		r.out[id] = p
		go r.pusher(pctx, p)
		go r.antiEntropyLoop(pctx, p)
	}
	for id, p := range r.out {
		if _, ok := want[id]; !ok {
			p.cancel()
			delete(r.out, id)
		}
	}
}

func (r *replicator) pusher(ctx context.Context, p *peerOut) {
	for {
		var batch []HB
		select {
		case <-ctx.Done():
			return
		case h := <-p.ch:
			batch = append(batch, h)
		}
	drain:
		for len(batch) < 500 {
			select {
			case h := <-p.ch:
				batch = append(batch, h)
			default:
				break drain
			}
		}
		body, _ := json.Marshal(batch)
		err := r.post(ctx, p.st.Addr, "/repl/push", body)
		p.mu.Lock()
		if err != nil {
			p.st.PushErrors++
			wasReachable := p.st.Reachable
			p.st.Reachable = false
			p.mu.Unlock()
			if wasReachable {
				slog.Warn("hb push failed, relying on anti-entropy", "peer", p.st.Addr, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		p.st.Pushed += int64(len(batch))
		recovered := !p.st.Reachable
		p.st.Reachable = true
		p.mu.Unlock()
		if recovered {
			select { // anti-entropy right after reconnect
			case p.trigger <- struct{}{}:
			default:
			}
		}
	}
}

func (r *replicator) antiEntropyLoop(ctx context.Context, p *peerOut) {
	t := time.NewTicker(r.aeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.trigger:
		}
		start := time.Now()
		n, err := r.antiEntropy(ctx, p.st.Addr)
		p.mu.Lock()
		p.st.AERuns++
		p.st.AEFetched += n
		if err != nil {
			p.st.LastAEError = err.Error()
		} else {
			p.st.LastAEError = ""
		}
		p.mu.Unlock()
		if n > 0 {
			slog.Info("anti-entropy filled gap", "peer", p.st.Addr, "new_rows", n, "ms", time.Since(start).Milliseconds())
		}
	}
}

// antiEntropy pulls everything the peer has beyond our contiguous watermark,
// for every (node, incarnation) stream the peer knows (including third parties).
func (r *replicator) antiEntropy(ctx context.Context, addr string) (int64, error) {
	var theirs map[string]Watermark
	if err := r.get(ctx, addr, "/repl/watermarks", &theirs); err != nil {
		return 0, err
	}
	mine, err := r.st.watermarks()
	if err != nil {
		return 0, err
	}
	var added int64
	for k, tw := range theirs {
		after := mine[k].Contig
		for tw.Max > after {
			q := url.Values{"node": {tw.NodeID}, "inc": {tw.Inc}, "after": {fmt.Sprint(after)}, "limit": {fmt.Sprint(r.pageSize)}}
			var page []HB
			if err := r.get(ctx, addr, "/repl/fetch?"+q.Encode(), &page); err != nil {
				return added, err
			}
			if len(page) == 0 {
				break
			}
			n, err := r.st.insert(page)
			if err != nil {
				return added, err
			}
			added += n
			after = page[len(page)-1].Seq
		}
	}
	return added, nil
}

func (r *replicator) stats() map[string]peerStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]peerStats{}
	for id, p := range r.out {
		p.mu.Lock()
		out[id] = p.st
		p.mu.Unlock()
	}
	return out
}

func (r *replicator) post(ctx context.Context, addr, path string, body []byte) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func (r *replicator) get(ctx context.Context, addr, path string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+path, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
