package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/raft"
)

const (
	schemeHTTPS = "https://"
	headerXHops = "X-Hops"
)

type writeResult struct {
	Status    int          `json:"-"`
	Result    *ApplyResult `json:"result,omitempty"`
	Error     string       `json:"error,omitempty"`
	Unknown   bool         `json:"result_unknown,omitempty"` // FA-CLU-06: 503 + retry with same request id
	AppliedBy string       `json:"applied_by,omitempty"`
	Forwarded bool         `json:"forwarded"`
	Ms        int64        `json:"ms"`
}

// write applies cmd on the leader or forwards it there (FA-CLU-05). The
// leader stamps time and UUIDv7 before raft.Apply (FA-CLU-07).
func (n *Node) write(ctx context.Context, cmd Command, hops int) writeResult {
	start := time.Now()
	res := n.writeInner(ctx, cmd, hops)
	res.Ms = time.Since(start).Milliseconds()
	return res
}

func (n *Node) writeInner(ctx context.Context, cmd Command, hops int) writeResult {
	if n.raft.State() == raft.Leader {
		cmd.IssuedAtMs = time.Now().UnixMilli()
		v7, _ := uuid.NewV7()
		cmd.VersionID = v7.String()
		data, _ := json.Marshal(cmd)
		f := n.raft.Apply(data, 5*time.Second)
		if err := f.Error(); err != nil {
			switch {
			case errors.Is(err, raft.ErrNotLeader), errors.Is(err, raft.ErrEnqueueTimeout):
				// not in the log: safe to retry anywhere
				return writeResult{Status: http.StatusServiceUnavailable, Error: err.Error()}
			case errors.Is(err, raft.ErrLeadershipLost), errors.Is(err, raft.ErrRaftShutdown):
				return writeResult{Status: http.StatusServiceUnavailable, Error: err.Error(), Unknown: true}
			default:
				return writeResult{Status: http.StatusInternalServerError, Error: err.Error(), Unknown: true}
			}
		}
		ar := f.Response().(ApplyResult)
		if ar.Err != "" {
			return writeResult{Status: http.StatusInternalServerError, Result: &ar, Error: ar.Err}
		}
		return writeResult{Status: http.StatusOK, Result: &ar, AppliedBy: n.cfg.Name}
	}
	if hops > 0 {
		return writeResult{Status: http.StatusServiceUnavailable, Error: "forwarded to a non-leader"}
	}
	addr, _ := n.raft.LeaderWithID()
	if addr == "" {
		return writeResult{Status: http.StatusServiceUnavailable, Error: "no leader (minority partition or election running)"}
	}
	body, _ := json.Marshal(cmd)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, schemeHTTPS+string(addr)+"/cluster/apply", bytes.NewReader(body))
	req.Header.Set(headerXHops, strconv.Itoa(hops+1))
	resp, err := n.client.Do(req)
	if err != nil {
		// The request may or may not have reached the leader.
		return writeResult{Status: http.StatusServiceUnavailable, Error: "forward: " + err.Error(), Unknown: true, Forwarded: true}
	}
	defer resp.Body.Close()
	var wr writeResult
	_ = json.NewDecoder(resp.Body).Decode(&wr)
	wr.Status = resp.StatusCode
	wr.Forwarded = true
	return wr
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- inter-node API (mTLS, HTTP/2) ----

func (n *Node) clusterHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /cluster/apply", func(w http.ResponseWriter, r *http.Request) {
		var cmd Command
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		hops, _ := strconv.Atoi(r.Header.Get(headerXHops))
		res := n.write(r.Context(), cmd, hops)
		writeJSON(w, res.Status, res)
	})
	m.HandleFunc("POST /cluster/join", n.handleJoin)
	m.HandleFunc("GET /cluster/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"raft_applied_index": n.raft.AppliedIndex(), "proto": r.Proto})
	})
	m.HandleFunc("POST /repl/push", func(w http.ResponseWriter, r *http.Request) {
		var batch []HB
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&batch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if _, err := n.hb.insert(batch); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	m.HandleFunc("GET /repl/watermarks", func(w http.ResponseWriter, r *http.Request) {
		wm, err := n.hb.watermarks()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, wm)
	})
	m.HandleFunc("GET /repl/fetch", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 5000 {
			limit = 5000
		}
		page, err := n.hb.fetchRange(q.Get("node"), q.Get("inc"), after, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	return m
}

type joinRequest struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
	Name string `json:"name"`
}

type joinResponse struct {
	Error          string `json:"error,omitempty"`
	ConflictID     string `json:"conflict_id,omitempty"`
	WaitedMutexMs  int64  `json:"waited_mutex_ms"`
	NonvoterMs     int64  `json:"nonvoter_ms"`
	CatchUpMs      int64  `json:"catch_up_ms"`
	PromoteMs      int64  `json:"promote_ms"`
	AlreadyMember  bool   `json:"already_member,omitempty"`
	LeaderName     string `json:"leader"`
	LeaderStartLag uint64 `json:"leader_last_index_after_nonvoter"`
	JoinerApplied  uint64 `json:"joiner_applied_at_promotion"`
}

func (n *Node) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, joinResponse{Error: err.Error()})
		return
	}
	if n.raft.State() != raft.Leader {
		n.forwardJoin(w, r, req)
		return
	}

	t0 := time.Now()
	n.memberMu.Lock()
	defer n.memberMu.Unlock()
	res := joinResponse{LeaderName: n.cfg.Name, WaitedMutexMs: time.Since(t0).Milliseconds()}
	slog.Info("join: start", "joiner", stripLineBreaks(req.Name), "id", stripLineBreaks(req.ID), "waited_mutex_ms", res.WaitedMutexMs)

	isMember, done := n.checkExistingMember(w, req, &res)
	if done {
		return
	}

	t1 := time.Now()
	addIndex := n.raft.LastIndex()
	if !isMember {
		f := n.raft.AddNonvoter(raft.ServerID(req.ID), raft.ServerAddress(req.Addr), 0, 10*time.Second)
		if err := f.Error(); err != nil {
			res.Error = "add nonvoter: " + err.Error()
			writeJSON(w, http.StatusServiceUnavailable, res)
			return
		}
		addIndex = f.Index()
	}
	res.NonvoterMs = time.Since(t1).Milliseconds()
	res.LeaderStartLag = n.raft.LastIndex()
	slog.Info("join: nonvoter added", "joiner", stripLineBreaks(req.Name), "ms", res.NonvoterMs)

	if !n.waitForJoinerCatchUp(r.Context(), w, req, addIndex, &res) {
		return
	}

	t3 := time.Now()
	if err := n.raft.AddVoter(raft.ServerID(req.ID), raft.ServerAddress(req.Addr), 0, 10*time.Second).Error(); err != nil {
		res.Error = "promote: " + err.Error()
		writeJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	res.PromoteMs = time.Since(t3).Milliseconds()
	slog.Info("join: promoted to voter", "joiner", stripLineBreaks(req.Name), "catch_up_ms", res.CatchUpMs, "promote_ms", res.PromoteMs)
	writeJSON(w, http.StatusOK, res)
}

// stripLineBreaks removes CR and LF from a request value before it is logged,
// so a request cannot forge log entries.
func stripLineBreaks(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", ""), "\r", "")
}

func (n *Node) forwardJoin(w http.ResponseWriter, r *http.Request, req joinRequest) {
	addr, _ := n.raft.LeaderWithID()
	if addr == "" || r.Header.Get(headerXHops) != "" {
		writeJSON(w, http.StatusServiceUnavailable, joinResponse{Error: "no leader"})
		return
	}
	body, _ := json.Marshal(req)
	fr, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, schemeHTTPS+string(addr)+"/cluster/join", bytes.NewReader(body))
	fr.Header.Set(headerXHops, "1")
	resp, err := n.client.Do(fr)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, joinResponse{Error: err.Error()})
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (n *Node) checkExistingMember(w http.ResponseWriter, req joinRequest, res *joinResponse) (isMember, done bool) {
	cf := n.raft.GetConfiguration()
	if err := cf.Error(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, joinResponse{Error: err.Error()})
		return false, true
	}
	for _, s := range cf.Configuration().Servers {
		if string(s.ID) == req.ID {
			if s.Suffrage == raft.Voter {
				res.AlreadyMember = true
				writeJSON(w, http.StatusOK, *res)
				return false, true
			}
			isMember = true
		} else if string(s.Address) == req.Addr {
			// Same address, different ID: a wiped node. raft rejects duplicate
			// addresses; never remove automatically (FA-CLU-08) -> operator.
			res.Error = "address already used by another server id; remove it first"
			res.ConflictID = string(s.ID)
			slog.Warn("join: address conflict", "joiner", stripLineBreaks(req.Name), "new_id", stripLineBreaks(req.ID), "old_id", s.ID)
			writeJSON(w, http.StatusConflict, *res)
			return false, true
		}
	}
	return isMember, false
}

func (n *Node) waitForJoinerCatchUp(ctx context.Context, w http.ResponseWriter, req joinRequest, addIndex uint64, res *joinResponse) bool {
	// hashicorp/raft does not expose per-follower match index, so ask the joiner.
	// A pure "lag < N entries" check is not enough: with a short log (or one
	// big entry / pending InstallSnapshot) an empty joiner passes immediately.
	// Require the joiner to have applied at least its own AddNonvoter entry.
	t2 := time.Now()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		applied, err := n.remoteApplied(ctx, req.Addr)
		if err == nil && applied >= addIndex && applied+n.cfg.CatchUpLag >= n.raft.LastIndex() {
			res.JoinerApplied = applied
			break
		}
		if time.Now().After(deadline) || n.raft.State() != raft.Leader {
			res.Error = fmt.Sprintf("catch-up failed (err=%v)", err)
			writeJSON(w, http.StatusServiceUnavailable, *res)
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
	if d := n.cfg.JoinHold - time.Since(t2); d > 0 {
		time.Sleep(d)
	}
	res.CatchUpMs = time.Since(t2).Milliseconds()
	return true
}

func (n *Node) remoteApplied(ctx context.Context, addr string) (uint64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, schemeHTTPS+addr+"/cluster/status", nil)
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var st struct {
		Applied uint64 `json:"raft_applied_index"`
	}
	return st.Applied, json.NewDecoder(resp.Body).Decode(&st)
}

// joinLoop runs on a node without raft state that has CLUSTER_JOIN set.
func (n *Node) joinLoop() {
	body, _ := json.Marshal(joinRequest{ID: n.nodeID, Addr: n.cfg.Advertise, Name: n.cfg.Name})
	for attempt := 1; ; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, schemeHTTPS+n.cfg.Join+"/cluster/join", bytes.NewReader(body))
		c := *n.client
		c.Timeout = 3 * time.Minute
		resp, err := c.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			slog.Info("join attempt", "attempt", attempt, "status", resp.StatusCode, "body", string(bytes.TrimSpace(b)))
			if resp.StatusCode == http.StatusOK {
				return
			}
		} else {
			slog.Warn("join attempt failed", "attempt", attempt, "err", err)
		}
		time.Sleep(2 * time.Second)
	}
}

// ---- admin API (plain HTTP, scenario script only) ----

func (n *Node) adminHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		leaderAddr, leaderID := n.raft.LeaderWithID()
		var servers []map[string]string
		if f := n.raft.GetConfiguration(); f.Error() == nil {
			for _, s := range f.Configuration().Servers {
				servers = append(servers, map[string]string{"id": string(s.ID), "addr": string(s.Address), "suffrage": s.Suffrage.String()})
			}
		}
		st := n.raft.Stats()
		writeJSON(w, http.StatusOK, map[string]any{
			"name": n.cfg.Name, "node_id": n.nodeID, "incarnation": n.hb.inc,
			"state": n.raft.State().String(), "term": st["term"], "last_contact": st["last_contact"],
			"leader_addr": leaderAddr, "leader_id": leaderID,
			"last_index": n.raft.LastIndex(), "commit_index": n.raft.CommitIndex(), "applied_index": n.raft.AppliedIndex(),
			"servers": servers, "fsm": n.fsm.Stats(), "boot": n.bootInfo, "hb_peers": n.repl.stats(),
			"uptime_ms": time.Since(n.startedAt).Milliseconds(),
		})
	})
	m.HandleFunc("PUT /kv/{key}", func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = uuid.NewString()
		}
		res := n.write(r.Context(), Command{RequestID: rid, Op: "set", Key: r.PathValue("key"), Value: r.URL.Query().Get("value")}, 0)
		writeJSON(w, res.Status, res)
	})
	m.HandleFunc("GET /kv/{key}", func(w http.ResponseWriter, r *http.Request) {
		v, ver, ok := n.fsm.Get(r.PathValue("key"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"value": v, "version_id": ver, "served_by": n.cfg.Name})
	})
	m.HandleFunc("POST /admin/bulk", func(w http.ResponseWriter, r *http.Request) {
		cnt, _ := strconv.Atoi(r.URL.Query().Get("n"))
		res := n.write(r.Context(), Command{RequestID: uuid.NewString(), Op: "bulk", Key: r.URL.Query().Get("prefix"), N: cnt}, 0)
		writeJSON(w, res.Status, res)
	})
	m.HandleFunc("POST /admin/snapshot", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		err := n.raft.Snapshot().Error()
		writeJSON(w, http.StatusOK, map[string]any{"err": fmt.Sprint(err), "ms": time.Since(start).Milliseconds()})
	})
	m.HandleFunc("POST /admin/remove", func(w http.ResponseWriter, r *http.Request) {
		if n.raft.State() != raft.Leader {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not leader"})
			return
		}
		n.memberMu.Lock()
		defer n.memberMu.Unlock()
		err := n.raft.RemoveServer(raft.ServerID(r.URL.Query().Get("id")), 0, 10*time.Second).Error()
		writeJSON(w, http.StatusOK, map[string]any{"err": fmt.Sprint(err)})
	})
	m.HandleFunc("GET /hb/summary", func(w http.ResponseWriter, r *http.Request) {
		wm, err := n.hb.watermarks()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, wm)
	})
	return m
}
