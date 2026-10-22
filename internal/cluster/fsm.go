// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store"
)

// statusHistoryRetention is how long consensus status changes are kept,
// measured in command time (Pflichtenheft 7.3).
const statusHistoryRetention = 30 * 24 * time.Hour

// dedupRetention is how long applied request IDs are remembered, measured in
// command time (issued_at_ms), so pruning is deterministic (ADR 0005 point 3).
const dedupRetention = 24 * time.Hour

// ErrFSMHalted is returned for every command after the FSM stopped because of
// an infrastructure error (ADR 0012 A7, FA-CLU-13).
var ErrFSMHalted = errors.New("state machine halted")

// StateStore is the storage the FSM materializes into.
type StateStore interface {
	store.State
	WriteSnapshot(ctx context.Context, w io.Writer) error
	RestoreSnapshot(ctx context.Context, r io.Reader) error
}

// ApplyResult is what FSM.Apply returns for a log entry.
type ApplyResult struct {
	Index uint64
	// Result is the command-specific result as JSON (e.g. a PutResult).
	Result json.RawMessage
	// Duplicate is true if the request ID had been applied before; Result and
	// Err are then those of the first application.
	Duplicate bool
	// Err is a *CommandError for deterministic rejections, or ErrFSMHalted.
	Err error
}

// FSM applies Raft log entries to state.db. Apply is deterministic: it reads
// neither the clock nor a random source; time and IDs come with the command.
type FSM struct {
	state   StateStore
	log     *slog.Logger
	onFatal func(error)

	applied  atomic.Uint64
	haltErr  atomic.Pointer[error]
	haltOnce sync.Once
	// changes signals (coalesced) that state.db changed; it is a notification
	// outside the replicated state and does not affect determinism.
	changes chan struct{}
	// observer receives the StateEvents of committed transactions (see OnStateChange).
	observer atomic.Pointer[func(StateEvent)]
}

var _ raft.FSM = (*FSM)(nil)

// NewFSM creates the FSM. onFatal is called once when the FSM halts; the
// caller is expected to stop the node (FA-CLU-13).
func NewFSM(ctx context.Context, state StateStore, log *slog.Logger, onFatal func(error)) (*FSM, error) {
	idx, err := state.AppliedIndex(ctx)
	if err != nil {
		return nil, err
	}
	f := &FSM{state: state, log: log, onFatal: onFatal, changes: make(chan struct{}, 1)}
	f.applied.Store(idx)
	return f, nil
}

// Changes returns a channel that receives a value after state.db changed. Several
// changes may be coalesced into one signal.
func (f *FSM) Changes() <-chan struct{} { return f.changes }

func (f *FSM) notify() {
	select {
	case f.changes <- struct{}{}:
	default:
	}
}

// AppliedIndex returns the last log index reflected in state.db.
func (f *FSM) AppliedIndex() uint64 { return f.applied.Load() }

// Healthy returns the halt reason, or nil while the FSM works.
func (f *FSM) Healthy() error {
	if p := f.haltErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (f *FSM) halt(err error) {
	f.haltOnce.Do(func() {
		wrapped := fmt.Errorf("%w: %w", ErrFSMHalted, err)
		f.haltErr.Store(&wrapped)
		f.log.Error("state machine halted; this node must be stopped and investigated", "err", err)
		if f.onFatal != nil {
			f.onFatal(wrapped)
		}
	})
}

// Apply implements raft.FSM.
func (f *FSM) Apply(l *raft.Log) any {
	if err := f.Healthy(); err != nil {
		return ApplyResult{Index: l.Index, Err: err}
	}
	// Entries up to the stored applied index are already in state.db; Raft
	// replays them after a restart (ADR 0012 A2).
	if l.Index <= f.applied.Load() {
		return ApplyResult{Index: l.Index}
	}

	ctx := context.Background()
	var cmd pb.Command
	if err := proto.Unmarshal(l.Data, &cmd); err != nil {
		return f.reject(ctx, l.Index, nil, invalid("decode command: %v", err))
	}
	if cmd.GetVersion() > CommandVersion {
		// Feature gates must prevent this; applying partially would diverge.
		f.halt(fmt.Errorf("command version %d at index %d is newer than supported version %d", cmd.GetVersion(), l.Index, CommandVersion))
		return ApplyResult{Index: l.Index, Err: f.Healthy()}
	}

	var res ApplyResult
	var collected stateEventCollector
	err := f.state.Update(ctx, func(tx store.StateTx) error {
		collected.events = nil
		txCtx := withStateEvents(ctx, &collected)
		if id := cmd.GetRequestId(); id != "" {
			e, err := tx.GetDedup(ctx, id)
			switch {
			case err == nil:
				res = resultFromDedup(l.Index, e)
				return tx.SetAppliedIndex(ctx, l.Index)
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		result, err := applyPayload(txCtx, tx, &cmd)
		if err != nil {
			return err
		}
		res = ApplyResult{Index: l.Index, Result: result}
		return f.finish(ctx, tx, l.Index, &cmd, store.DedupEntry{Result: result})
	})

	var cmdErr *CommandError
	switch {
	case err == nil:
	case errors.As(err, &cmdErr):
		// The payload transaction was rolled back; record the rejection.
		return f.reject(ctx, l.Index, &cmd, cmdErr)
	default:
		f.halt(fmt.Errorf("apply index %d: %w", l.Index, err))
		return ApplyResult{Index: l.Index, Err: f.Healthy()}
	}
	f.applied.Store(l.Index)
	if !res.Duplicate {
		f.notify()
		f.emit(collected.events)
	}
	return res
}

// reject stores a deterministic command error and advances the applied index.
func (f *FSM) reject(ctx context.Context, index uint64, cmd *pb.Command, cmdErr *CommandError) ApplyResult {
	err := f.state.Update(ctx, func(tx store.StateTx) error {
		return f.finish(ctx, tx, index, cmd, store.DedupEntry{ErrorCode: cmdErr.Code, ErrorMessage: cmdErr.Message})
	})
	if err != nil {
		f.halt(fmt.Errorf("record rejection at index %d: %w", index, err))
		return ApplyResult{Index: index, Err: f.Healthy()}
	}
	f.applied.Store(index)
	return ApplyResult{Index: index, Err: cmdErr}
}

// finish writes the dedup entry, prunes old ones and stores the applied index
// in the same transaction as the data (ADR 0012 A1).
func (f *FSM) finish(ctx context.Context, tx store.StateTx, index uint64, cmd *pb.Command, e store.DedupEntry) error {
	if cmd != nil && cmd.GetRequestId() != "" {
		e.RequestID = cmd.GetRequestId()
		e.AppliedIndex = index
		e.IssuedAtMs = cmd.GetIssuedAtMs()
		if err := tx.PutDedup(ctx, e); err != nil {
			return err
		}
		if err := tx.PruneDedup(ctx, cmd.GetIssuedAtMs()-dedupRetention.Milliseconds()); err != nil {
			return err
		}
	}
	return tx.SetAppliedIndex(ctx, index)
}

func resultFromDedup(index uint64, e store.DedupEntry) ApplyResult {
	res := ApplyResult{Index: index, Result: e.Result, Duplicate: true}
	if e.ErrorCode != "" {
		res.Err = &CommandError{Code: e.ErrorCode, Message: e.ErrorMessage}
	}
	return res
}

// applyPayload executes one command. Deterministic rejections are returned as
// *CommandError; any other error is an infrastructure failure.
func applyPayload(ctx context.Context, tx store.StateTx, cmd *pb.Command) (json.RawMessage, error) {
	switch p := cmd.GetPayload().(type) {
	case *pb.Command_PutEntity:
		return applyPut(ctx, tx, cmd, p.PutEntity)
	case *pb.Command_DeleteEntity:
		return applyDelete(ctx, tx, cmd, p.DeleteEntity)
	case *pb.Command_SetMonitorState:
		return applySetMonitorState(ctx, tx, cmd, p.SetMonitorState)
	case *pb.Command_EnqueueAlert:
		return applyEnqueueAlert(ctx, tx, cmd, p.EnqueueAlert)
	case *pb.Command_AckAlertDelivery:
		return applyAckAlertDelivery(ctx, tx, cmd, p.AckAlertDelivery)
	case *pb.Command_CreateSession:
		return applyCreateSession(ctx, tx, cmd, p.CreateSession)
	case *pb.Command_RotateSession:
		return applyRotateSession(ctx, tx, cmd, p.RotateSession)
	case *pb.Command_RevokeSessions:
		return applyRevokeSessions(ctx, tx, cmd, p.RevokeSessions)
	case *pb.Command_RedeemJoinToken:
		return applyRedeemJoinToken(ctx, tx, cmd, p.RedeemJoinToken)
	case *pb.Command_RevokeCertificate:
		return applyRevokeCertificate(ctx, tx, cmd, p.RevokeCertificate)
	case nil:
		return nil, invalid("command without payload")
	default:
		return nil, &CommandError{Code: CodeUnsupported, Message: fmt.Sprintf("payload %T is not supported yet", p)}
	}
}

func applyPut(ctx context.Context, tx store.StateTx, cmd *pb.Command, put *pb.PutEntity) (json.RawMessage, error) {
	if put.GetId() == "" {
		return nil, invalid("put %s without ID", put.GetKind())
	}
	issuedAt := cmd.GetIssuedAtMs()

	switch put.GetKind() {
	case pb.EntityKind_ENTITY_KIND_TENANT:
		var doc TenantDocument
		if err := json.Unmarshal(put.GetDocumentJson(), &doc); err != nil {
			return nil, invalid("tenant document: %v", err)
		}
		err := tx.PutTenant(ctx, store.Tenant{
			ID: put.GetId(), Name: doc.Name, Plan: doc.Plan, ZitadelOrgID: doc.ZitadelOrgID,
			Limits: doc.Limits, Suspended: doc.Suspended, CreatedMs: issuedAt, UpdatedMs: issuedAt,
		})
		if err != nil {
			return nil, storeError(err)
		}
		return json.Marshal(PutResult{ID: put.GetId()})

	case pb.EntityKind_ENTITY_KIND_MONITOR:
		if cmd.GetTenantId() == "" {
			return nil, invalid("monitor command without tenant")
		}
		var doc MonitorDocument
		if err := json.Unmarshal(put.GetDocumentJson(), &doc); err != nil {
			return nil, invalid("monitor document: %v", err)
		}
		if err := checkMonitorSecrets(doc.Secrets); err != nil {
			return nil, err
		}
		create := put.GetExpectedRevision() == 0
		err := tx.PutMonitor(ctx, store.Monitor{
			TenantID:              cmd.GetTenantId(),
			ID:                    put.GetId(),
			Name:                  doc.Name,
			Description:           doc.Description,
			Type:                  doc.Type,
			Config:                doc.Config,
			IntervalSeconds:       doc.IntervalSeconds,
			TimeoutSeconds:        doc.TimeoutSeconds,
			Retries:               doc.Retries,
			RecoveryThreshold:     doc.RecoveryThreshold,
			ResendIntervalMinutes: doc.ResendIntervalMinutes,
			Assignment:            doc.Assignment,
			ConsensusPolicy:       doc.ConsensusPolicy,
			Tags:                  doc.Tags,
			ChannelIDs:            doc.NotificationChannelIDs,
			Secrets:               doc.Secrets,
			Paused:                doc.Paused,
			CreatedMs:             issuedAt,
			UpdatedMs:             issuedAt,
		}, put.GetExpectedRevision())
		if err != nil {
			return nil, storeError(err)
		}
		if err := syncPausedState(ctx, tx, cmd.GetTenantId(), put.GetId(), doc.Paused, create, issuedAt); err != nil {
			return nil, err
		}
		return json.Marshal(PutResult{ID: put.GetId(), Revision: put.GetExpectedRevision() + 1})

	case pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL:
		var doc ChannelDocument
		if err := json.Unmarshal(put.GetDocumentJson(), &doc); err != nil {
			return nil, invalid("channel document: %v", err)
		}
		ch, err := channelFromDocument(cmd.GetTenantId(), put.GetId(), doc, issuedAt)
		if err != nil {
			return nil, err
		}
		if err := tx.PutChannel(ctx, ch, put.GetExpectedRevision()); err != nil {
			return nil, storeError(err)
		}
		return json.Marshal(PutResult{ID: put.GetId(), Revision: put.GetExpectedRevision() + 1})

	case pb.EntityKind_ENTITY_KIND_MAINTENANCE_WINDOW:
		var doc MaintenanceDocument
		if err := json.Unmarshal(put.GetDocumentJson(), &doc); err != nil {
			return nil, invalid("maintenance document: %v", err)
		}
		w, err := maintenanceFromDocument(cmd.GetTenantId(), put.GetId(), doc, issuedAt)
		if err != nil {
			return nil, err
		}
		if err := tx.PutMaintenanceWindow(ctx, w, put.GetExpectedRevision()); err != nil {
			return nil, storeError(err)
		}
		return json.Marshal(PutResult{ID: put.GetId(), Revision: put.GetExpectedRevision() + 1})

	case pb.EntityKind_ENTITY_KIND_USER:
		return applyPutUser(ctx, tx, cmd, put)

	case pb.EntityKind_ENTITY_KIND_SIGNING_KEY:
		return applyPutSigningKey(ctx, tx, cmd, put)

	case pb.EntityKind_ENTITY_KIND_CLUSTER_CA:
		return applyPutClusterCA(ctx, tx, cmd, put)

	case pb.EntityKind_ENTITY_KIND_JOIN_TOKEN:
		return applyPutJoinToken(ctx, tx, cmd, put)

	case pb.EntityKind_ENTITY_KIND_SECRET_KEY:
		var doc SecretKeyDocument
		if err := json.Unmarshal(put.GetDocumentJson(), &doc); err != nil {
			return nil, invalid("secret key document: %v", err)
		}
		if len(doc.WrappedDEK) == 0 || doc.KEKFingerprint == "" {
			return nil, invalid("secret key without wrapped key or master key fingerprint")
		}
		err := tx.PutSecretKey(ctx, store.SecretKey{
			KeyID: put.GetId(), WrappedDEK: doc.WrappedDEK, KEKFingerprint: doc.KEKFingerprint,
			CreatedMs: issuedAt, RetiredMs: doc.RetiredMs,
		})
		if err != nil {
			return nil, storeError(err)
		}
		return json.Marshal(PutResult{ID: put.GetId()})

	default:
		return nil, &CommandError{Code: CodeUnsupported, Message: fmt.Sprintf("put of %s is not supported yet", put.GetKind())}
	}
}

func applyDelete(ctx context.Context, tx store.StateTx, cmd *pb.Command, del *pb.DeleteEntity) (json.RawMessage, error) {
	switch del.GetKind() {
	case pb.EntityKind_ENTITY_KIND_MONITOR:
		if err := tx.DeleteMonitor(ctx, cmd.GetTenantId(), del.GetId()); err != nil {
			return nil, storeError(err)
		}
		return json.Marshal(PutResult{ID: del.GetId()})
	case pb.EntityKind_ENTITY_KIND_USER:
		return applyDeleteUser(ctx, tx, cmd, del.GetId())
	case pb.EntityKind_ENTITY_KIND_JOIN_TOKEN:
		return applyDeleteJoinToken(ctx, tx, del.GetId())
	case pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL:
		if err := tx.DeleteChannel(ctx, cmd.GetTenantId(), del.GetId()); err != nil {
			return nil, storeError(err)
		}
		return json.Marshal(PutResult{ID: del.GetId()})
	case pb.EntityKind_ENTITY_KIND_MAINTENANCE_WINDOW:
		if err := tx.DeleteMaintenanceWindow(ctx, cmd.GetTenantId(), del.GetId()); err != nil {
			return nil, storeError(err)
		}
		return json.Marshal(PutResult{ID: del.GetId()})
	default:
		return nil, &CommandError{Code: CodeUnsupported, Message: fmt.Sprintf("delete of %s is not supported yet", del.GetKind())}
	}
}

// syncPausedState keeps the monitor state in line with the paused flag: a new
// monitor starts PENDING (or PAUSED), pausing ends an open incident, and
// resuming starts a new PENDING phase.
func syncPausedState(ctx context.Context, tx store.StateTx, tenantID, monitorID string, paused, create bool, nowMs int64) error {
	next := store.MonitorState{Status: store.StatusPending, Confirmed: true, SinceMs: nowMs}
	if paused {
		next.Status = store.StatusPaused
	}
	resolved := false
	if !create {
		cur, err := tx.GetMonitorState(ctx, tenantID, monitorID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil && paused == (cur.Status == store.StatusPaused) {
			return nil
		}
		if err == nil && cur.Status == store.StatusDown {
			if err := tx.ResolveIncident(ctx, tenantID, monitorID, nowMs); err != nil {
				return err
			}
			resolved = true
		}
	}
	if err := tx.PutMonitorState(ctx, tenantID, monitorID, next); err != nil {
		return err
	}
	recordStateEvent(ctx, StateEvent{TenantID: tenantID, MonitorID: monitorID, Status: next.Status,
		Confirmed: next.Confirmed, SinceMs: next.SinceMs, IncidentResolved: resolved})
	return tx.PruneStatusHistory(ctx, tenantID, monitorID, nowMs-statusHistoryRetention.Milliseconds())
}

// applySetMonitorState records a status change decided by the leader and
// opens or resolves the incident with it (FA-CON-06, FA-INC-01).
func applySetMonitorState(ctx context.Context, tx store.StateTx, cmd *pb.Command, set *pb.SetMonitorState) (json.RawMessage, error) {
	tenantID, monitorID := cmd.GetTenantId(), set.GetMonitorId()
	to := store.MonitorStatus(set.GetStatus())
	switch {
	case tenantID == "" || monitorID == "":
		return nil, invalid("monitor state without tenant or monitor")
	case !to.Valid() || to == store.StatusPaused:
		return nil, invalid("monitor state %q cannot be set", to)
	case to == store.StatusDown && set.GetIncident().GetIncidentId() == "":
		return nil, invalid("change to DOWN without incident")
	}

	cur, err := tx.GetMonitorState(ctx, tenantID, monitorID)
	if err != nil {
		return nil, storeError(err)
	}
	if cur.Status == store.StatusPaused {
		return nil, &CommandError{Code: CodeConflict, Message: fmt.Sprintf("monitor %s is paused", monitorID)}
	}
	if cur.Status != store.MonitorStatus(set.GetExpectedStatus()) {
		return nil, &CommandError{Code: CodeConflict, Message: fmt.Sprintf("monitor %s is %s, not %s", monitorID, cur.Status, set.GetExpectedStatus())}
	}
	if cur.Status == to {
		return nil, invalid("monitor %s is already %s", monitorID, to)
	}
	// The history must stay ordered; a change never predates the current state.
	since := max(set.GetSinceMs(), cur.SinceMs)

	if cur.Status == store.StatusDown {
		if err := tx.ResolveIncident(ctx, tenantID, monitorID, since); err != nil {
			return nil, storeError(err)
		}
	}
	if to == store.StatusDown {
		inc := set.GetIncident()
		err := tx.OpenIncident(ctx, store.Incident{
			TenantID:      tenantID,
			ID:            inc.GetIncidentId(),
			MonitorID:     monitorID,
			StartedMs:     since,
			Cause:         inc.GetCause(),
			ErrorClass:    ErrorClassFromProto(inc.GetErrorClass()),
			AffectedNodes: inc.GetAffectedNodeIds(),
			Confirmed:     set.GetConfirmed(),
		})
		if err != nil {
			return nil, storeError(err)
		}
	}
	err = tx.PutMonitorState(ctx, tenantID, monitorID, store.MonitorState{Status: to, Confirmed: set.GetConfirmed(), SinceMs: since})
	if err != nil {
		return nil, storeError(err)
	}
	ev := StateEvent{TenantID: tenantID, MonitorID: monitorID, Status: to, Confirmed: set.GetConfirmed(),
		SinceMs: since, IncidentResolved: cur.Status == store.StatusDown}
	if to == store.StatusDown {
		ev.IncidentOpened = set.GetIncident().GetIncidentId()
	}
	recordStateEvent(ctx, ev)
	if err := tx.PruneStatusHistory(ctx, tenantID, monitorID, cmd.GetIssuedAtMs()-statusHistoryRetention.Milliseconds()); err != nil {
		return nil, err
	}
	if err := notifyStateChange(ctx, tx, cmd, set, cur.Status, to, since); err != nil {
		return nil, storeError(err)
	}
	return json.Marshal(StateResult{Status: to})
}

// StateResult is the result of a SetMonitorState command.
type StateResult struct {
	Status store.MonitorStatus `json:"status"`
}

// storeError turns deterministic store errors into command errors and passes
// everything else through as an infrastructure failure.
func storeError(err error) error {
	switch {
	case errors.Is(err, store.ErrConflict):
		return &CommandError{Code: CodeConflict, Message: err.Error()}
	case errors.Is(err, store.ErrNotFound):
		return &CommandError{Code: CodeNotFound, Message: err.Error()}
	}
	return err
}

// Snapshot implements raft.FSM. The copy is taken in Persist, concurrently with
// further Apply calls; that "fuzzy" snapshot is safe because the applied index
// is part of the copy (ADR 0012 A1).
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	return &fsmSnapshot{state: f.state}, nil
}

// Restore implements raft.FSM: it replaces state.db with the snapshot.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer func() { _ = rc.Close() }()
	ctx := context.Background()
	if err := f.state.RestoreSnapshot(ctx, rc); err != nil {
		// An invalid snapshot leaves the old state untouched, but the node no
		// longer matches the cluster; stop instead of serving wrong data.
		f.halt(fmt.Errorf("restore snapshot: %w", err))
		return err
	}
	idx, err := f.state.AppliedIndex(ctx)
	if err != nil {
		f.halt(fmt.Errorf("read applied index after restore: %w", err))
		return err
	}
	f.applied.Store(idx)
	f.notify()
	f.log.Info("restored state from snapshot", "applied_index", idx)
	return nil
}

type fsmSnapshot struct {
	state StateStore
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := s.state.WriteSnapshot(context.Background(), sink); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("persist snapshot: %w", err)
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// checkMonitorSecrets rejects secrets whose pointer is not a JSON pointer into
// Config or whose value is not a ciphertext (ADR 0015). Keys are visited in
// sorted order so that the error is deterministic.
func checkMonitorSecrets(fields map[string]string) error {
	pointers := make([]string, 0, len(fields))
	for p := range fields {
		pointers = append(pointers, p)
	}
	sort.Strings(pointers)
	for _, p := range pointers {
		if !strings.HasPrefix(p, "/") {
			return invalid("monitor secret pointer %q is invalid", p)
		}
		if !secrets.IsEncrypted(fields[p]) {
			return invalid("monitor secret %s is not encrypted", p)
		}
	}
	return nil
}
