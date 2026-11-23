// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/configsecret"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/uptime"
)

// Monitor limits (FA-MON-03). The minimum interval becomes a per-plan limit
// in phase 4 (FA-TEN-04).
const (
	minIntervalSeconds = 20
	maxIntervalSeconds = 86400
	maxTimeoutSeconds  = 48
	maxTags            = 20
	maxTagLength       = 64
	maxNameLength      = 200
	maxDescription     = 2000
)

type access int

const (
	readAccess access = iota
	editAccess
	adminAccess
	// platformAccess is cluster administration (nodes, join tokens): only
	// platform admins, never tenant admins (FA-CLU-08).
	platformAccess
)

// principal returns the caller and checks the role (FA-AUTH-09).
func principal(ctx context.Context, need access) (auth.Principal, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return auth.Principal{}, &Problem{Status: http.StatusUnauthorized, Title: "Unauthorized"}
	}
	allowed := p.CanRead()
	switch need {
	case editAccess:
		allowed = p.CanEdit()
	case adminAccess:
		allowed = p.CanAdmin()
	case platformAccess:
		allowed = p.Has(auth.RolePlatformAdmin)
	}
	if !allowed {
		return auth.Principal{}, &Problem{Status: http.StatusForbidden, Title: "Forbidden", Detail: "your role does not allow this action"}
	}
	return p, nil
}

func notFound(what string) *Problem {
	return &Problem{Status: http.StatusNotFound, Title: "Not Found", Detail: what + " not found"}
}

// requestID scopes the client's Idempotency-Key to the tenant, so one tenant
// can never receive the deduplicated result of another (FA-CLU-06).
func requestID(tenantID string, key *uuid.UUID) string {
	if key == nil {
		return uuid.NewString()
	}
	return "api:" + tenantID + ":" + key.String()
}

// apply proposes a command and maps cluster errors to HTTP (ADR 0012 A8).
// Commands without an actor get the caller as actor, so the FSM can apply the
// token rules and write the audit log (ADR 0031).
func (h *Handlers) apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	if cmd.GetActor() == nil {
		if p, ok := auth.FromContext(ctx); ok {
			cmd.Actor = actorOf(p)
		}
	}
	res, err := h.deps.Cluster.Apply(ctx, cmd)
	if err == nil {
		return res, nil
	}
	var ce *cluster.CommandError
	switch {
	case errors.As(err, &ce):
		switch ce.Code {
		case cluster.CodeConflict:
			if tenantAdminCommand(cmd) {
				// The FSM messages of the tenant commands name the cause
				// (organization bound, ID of a deleted tenant, not suspended).
				return res, &Problem{Status: http.StatusConflict, Title: "Conflict", Detail: ce.Message}
			}
			if cmd.GetBatch() != nil {
				// The FSM names the operation and monitor (ADR 0031, section 3).
				return res, &Problem{Status: http.StatusConflict, Title: "Conflict",
					Detail: "nothing was applied; a monitor was changed concurrently: " + ce.Message}
			}
			return res, &Problem{Status: http.StatusConflict, Title: "Conflict", Detail: "the object was changed concurrently or already exists; reload and retry"}
		case cluster.CodeNotFound:
			return res, notFound("object")
		case cluster.CodeNotEnabled:
			return res, notEnabled(ce)
		case cluster.CodeLimitExceeded:
			return res, limitProblem(ce)
		default:
			return res, &Problem{Status: http.StatusUnprocessableEntity, Title: "Unprocessable Entity", Detail: ce.Message}
		}
	case errors.Is(err, cluster.ErrNotApplied):
		return res, &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable", Detail: "no leader available, retry", RetryAfter: 1}
	case errors.Is(err, cluster.ErrUnknownOutcome):
		return res, &Problem{Status: http.StatusServiceUnavailable, Title: "Result Unknown",
			Detail: "the change may or may not have been applied; retry with the same Idempotency-Key", RetryAfter: 1}
	case errors.Is(err, cluster.ErrFSMHalted):
		return res, &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable", Detail: "this node is halted"}
	}
	return res, err
}

// ListMonitors returns a page of the caller's monitors.
func (h *Handlers) ListMonitors(ctx context.Context, req gen.ListMonitorsRequestObject) (gen.ListMonitorsResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	q := store.MonitorQuery{}
	if req.Params.Limit != nil {
		q.Limit = *req.Params.Limit
	}
	if req.Params.Tag != nil {
		q.Tag = *req.Params.Tag
	}
	if req.Params.Q != nil {
		q.Search = *req.Params.Q
	}
	if req.Params.Status != nil {
		q.Status = store.MonitorStatus(*req.Params.Status)
	}
	if req.Params.Cursor != nil {
		if q.AfterID, err = decodeCursor(*req.Params.Cursor, "m"); err != nil {
			return nil, err
		}
	}
	page, err := h.deps.State.ListMonitors(ctx, p.TenantID, q)
	if err != nil {
		return nil, err
	}
	items, err := h.apiMonitors(ctx, page.Items)
	if err != nil {
		return nil, err
	}
	out := gen.MonitorPage{Items: items}
	if page.NextAfterID != "" {
		c := encodeCursor("m", page.NextAfterID)
		out.NextCursor = &c
	}
	return gen.ListMonitors200JSONResponse(out), nil
}

// GetMonitor returns one monitor.
func (h *Handlers) GetMonitor(ctx context.Context, req gen.GetMonitorRequestObject) (gen.GetMonitorResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	m, err := h.monitor(ctx, p, req.MonitorId)
	if err != nil {
		return nil, err
	}
	out, err := h.apiMonitor(ctx, m)
	if err != nil {
		return nil, err
	}
	return gen.GetMonitor200JSONResponse(out), nil
}

// CreateMonitor validates the input and creates the monitor through Raft.
func (h *Handlers) CreateMonitor(ctx context.Context, req gen.CreateMonitorRequestObject) (gen.CreateMonitorResponseObject, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return nil, err
	}
	doc, err := h.monitorDocument(ctx, p.TenantID, req.Body, true)
	if err != nil {
		return nil, err
	}
	if err := h.checkLimits(ctx, p.TenantID, pb.EntityKind_ENTITY_KIND_MONITOR, true, doc.IntervalSeconds, 0); err != nil {
		return nil, err
	}
	m, err := h.create(ctx, p, requestID(p.TenantID, req.Params.IdempotencyKey), "", func(id string) (cluster.MonitorDocument, error) {
		return h.sealSecrets(doc, id, nil)
	})
	if err != nil {
		return nil, err
	}
	out, err := h.apiMonitor(ctx, m)
	if err != nil {
		return nil, err
	}
	return gen.CreateMonitor201JSONResponse(out), nil
}

// create proposes a new monitor. prepare builds its document for the new ID,
// because secrets are encrypted for that ID (ADR 0015). hint is the audit
// action refinement ("clone" or empty, ADR 0031).
func (h *Handlers) create(ctx context.Context, p auth.Principal, reqID, hint string,
	prepare func(id string) (cluster.MonitorDocument, error)) (store.Monitor, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return store.Monitor{}, err
	}
	doc, err := prepare(id.String())
	if err != nil {
		return store.Monitor{}, err
	}
	cmd, err := cluster.NewPutMonitor(reqID, p.TenantID, id.String(), doc, 0)
	if err != nil {
		return store.Monitor{}, err
	}
	cmd.AuditAction = hint
	res, err := h.apply(ctx, cmd)
	if err != nil {
		return store.Monitor{}, err
	}
	// A deduplicated retry returns the ID of the first attempt.
	var put cluster.PutResult
	if err := json.Unmarshal(res.Result, &put); err != nil {
		return store.Monitor{}, fmt.Errorf("decode put result: %w", err)
	}
	return h.deps.State.GetMonitor(ctx, p.TenantID, put.ID)
}

// UpdateMonitor replaces the monitor's configuration.
func (h *Handlers) UpdateMonitor(ctx context.Context, req gen.UpdateMonitorRequestObject) (gen.UpdateMonitorResponseObject, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return nil, err
	}
	current, err := h.monitor(ctx, p, req.MonitorId)
	if err != nil {
		return nil, err
	}
	doc, err := h.monitorDocument(ctx, p.TenantID, req.Body, false)
	if err != nil {
		return nil, err
	}
	if err := h.checkLimits(ctx, p.TenantID, pb.EntityKind_ENTITY_KIND_MONITOR, false, doc.IntervalSeconds, current.IntervalSeconds); err != nil {
		return nil, err
	}
	var previous configsecret.Secrets
	if current.Type == doc.Type {
		previous = current.Secrets // a type change drops all stored secrets
	}
	if doc, err = h.sealSecrets(doc, current.ID, previous); err != nil {
		return nil, err
	}
	if err := h.validateResolved(ctx, doc, current.ID); err != nil {
		return nil, err
	}
	m, err := h.replace(ctx, p, current, doc, "")
	if err != nil {
		return nil, err
	}
	out, err := h.apiMonitor(ctx, m)
	if err != nil {
		return nil, err
	}
	return gen.UpdateMonitor200JSONResponse(out), nil
}

// replace stores doc as the new version of current. hint is the audit action
// refinement ("pause", "resume" or empty, ADR 0031).
func (h *Handlers) replace(ctx context.Context, p auth.Principal, current store.Monitor, doc cluster.MonitorDocument, hint string) (store.Monitor, error) {
	cmd, err := cluster.NewPutMonitor(uuid.NewString(), p.TenantID, current.ID, doc, current.Revision)
	if err != nil {
		return store.Monitor{}, err
	}
	cmd.AuditAction = hint
	if _, err := h.apply(ctx, cmd); err != nil {
		return store.Monitor{}, err
	}
	return h.deps.State.GetMonitor(ctx, p.TenantID, current.ID)
}

// DeleteMonitor removes the monitor; its measurement data expires with the
// retention (FA-MON-07).
func (h *Handlers) DeleteMonitor(ctx context.Context, req gen.DeleteMonitorRequestObject) (gen.DeleteMonitorResponseObject, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return nil, err
	}
	if _, err := h.monitor(ctx, p, req.MonitorId); err != nil {
		return nil, err
	}
	if _, err := h.apply(ctx, cluster.NewDeleteMonitor(uuid.NewString(), p.TenantID, req.MonitorId.String())); err != nil {
		return nil, err
	}
	return gen.DeleteMonitor204Response{}, nil
}

// PauseMonitor stops checks of a monitor (FA-MON-04).
func (h *Handlers) PauseMonitor(ctx context.Context, req gen.PauseMonitorRequestObject) (gen.PauseMonitorResponseObject, error) {
	m, err := h.setPaused(ctx, req.MonitorId, true)
	if err != nil {
		return nil, err
	}
	out, err := h.apiMonitor(ctx, m)
	if err != nil {
		return nil, err
	}
	return gen.PauseMonitor200JSONResponse(out), nil
}

// ResumeMonitor restarts checks of a paused monitor.
func (h *Handlers) ResumeMonitor(ctx context.Context, req gen.ResumeMonitorRequestObject) (gen.ResumeMonitorResponseObject, error) {
	m, err := h.setPaused(ctx, req.MonitorId, false)
	if err != nil {
		return nil, err
	}
	out, err := h.apiMonitor(ctx, m)
	if err != nil {
		return nil, err
	}
	return gen.ResumeMonitor200JSONResponse(out), nil
}

func (h *Handlers) setPaused(ctx context.Context, id uuid.UUID, paused bool) (store.Monitor, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return store.Monitor{}, err
	}
	current, err := h.monitor(ctx, p, id)
	if err != nil {
		return store.Monitor{}, err
	}
	if current.Paused == paused {
		return current, nil
	}
	doc := cluster.MonitorDocumentFrom(current)
	doc.Paused = paused
	hint := "resume"
	if paused {
		hint = "pause"
	}
	return h.replace(ctx, p, current, doc, hint)
}

// CloneMonitor copies a monitor; the copy starts paused (FA-MON-05).
func (h *Handlers) CloneMonitor(ctx context.Context, req gen.CloneMonitorRequestObject) (gen.CloneMonitorResponseObject, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return nil, err
	}
	src, err := h.monitor(ctx, p, req.MonitorId)
	if err != nil {
		return nil, err
	}
	doc := cluster.MonitorDocumentFrom(src)
	doc.Name = truncateRunes(src.Name, maxNameLength-len(" (copy)")) + " (copy)"
	doc.Paused = true
	if err := h.checkLimits(ctx, p.TenantID, pb.EntityKind_ENTITY_KIND_MONITOR, true, doc.IntervalSeconds, 0); err != nil {
		return nil, err
	}
	m, err := h.create(ctx, p, requestID(p.TenantID, req.Params.IdempotencyKey), "clone", func(id string) (cluster.MonitorDocument, error) {
		var err error
		doc.Secrets, err = configsecret.Reencrypt(ctx, h.keyring(), src.ID, id, src.Secrets)
		return doc, err
	})
	if err != nil {
		return nil, err
	}
	out, err := h.apiMonitor(ctx, m)
	if err != nil {
		return nil, err
	}
	return gen.CloneMonitor201JSONResponse(out), nil
}

// TestMonitor runs the check once on this node without storing the result.
func (h *Handlers) TestMonitor(ctx context.Context, req gen.TestMonitorRequestObject) (gen.TestMonitorResponseObject, error) {
	p, err := principal(ctx, editAccess)
	if err != nil {
		return nil, err
	}
	m, err := h.monitor(ctx, p, req.MonitorId)
	if err != nil {
		return nil, err
	}
	c, err := h.deps.Registry.Lookup(m.Type)
	if err != nil {
		return nil, &Problem{Status: http.StatusUnprocessableEntity, Title: "Unprocessable Entity", Detail: err.Error()}
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
	defer cancel()
	cfg, err := configsecret.ResolveMonitor(ctx, h.keyring(), m)
	if err != nil {
		return nil, fmt.Errorf("resolve secrets of monitor %s: %w", m.ID, err)
	}
	started := h.deps.Now()
	res := c.Check(checkCtx, h.deps.CheckEnv, cfg)
	return gen.TestMonitor200JSONResponse(toAPICheckResult(res, h.deps.NodeID, started)), nil
}

// ListHeartbeats returns raw heartbeats of a monitor, oldest first.
func (h *Handlers) ListHeartbeats(ctx context.Context, req gen.ListHeartbeatsRequestObject) (gen.ListHeartbeatsResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	if _, err := h.monitor(ctx, p, req.MonitorId); err != nil {
		return nil, err
	}
	q := store.HeartbeatQuery{MonitorID: req.MonitorId.String()}
	if req.Params.From != nil {
		q.FromMs = req.Params.From.UnixMilli()
	}
	if req.Params.To != nil {
		q.ToMs = req.Params.To.UnixMilli()
	}
	if req.Params.Node != nil {
		q.NodeID = *req.Params.Node
	}
	if req.Params.Limit != nil {
		q.Limit = *req.Params.Limit
	}
	if req.Params.Cursor != nil {
		raw, err := decodeCursor(*req.Params.Cursor, "h")
		if err != nil {
			return nil, err
		}
		var c store.HeartbeatCursor
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, &Problem{Status: http.StatusBadRequest, Title: problemTitleBadRequest, Detail: "invalid cursor"}
		}
		q.After = &c
	}
	page, err := h.deps.Metrics.ListHeartbeats(ctx, p.TenantID, q)
	if err != nil {
		return nil, err
	}
	out := gen.HeartbeatPage{Items: make([]gen.Heartbeat, 0, len(page.Items))}
	for _, hb := range page.Items {
		out.Items = append(out.Items, toAPIHeartbeat(hb))
	}
	if page.Next != nil {
		raw, _ := json.Marshal(page.Next)
		c := encodeCursor("h", string(raw))
		out.NextCursor = &c
	}
	return gen.ListHeartbeats200JSONResponse(out), nil
}

// ListMonitorTypes lists the checker plugins with their JSON Schema (FA-API-06).
func (h *Handlers) ListMonitorTypes(ctx context.Context, _ gen.ListMonitorTypesRequestObject) (gen.ListMonitorTypesResponseObject, error) {
	if _, err := principal(ctx, readAccess); err != nil {
		return nil, err
	}
	var out []gen.MonitorType
	for _, c := range h.deps.Registry.Checkers() {
		schema, err := schemaWithOrder(c.ConfigSchema())
		if err != nil {
			return nil, fmt.Errorf("schema of %s: %w", c.Type(), err)
		}
		t := gen.MonitorType{Type: c.Type(), Name: c.Name(), ConfigSchema: schema}
		if req, ok := c.(checker.CapabilityRequirer); ok {
			capability := req.RequiredCapability()
			t.RequiresCapability = &capability
		}
		out = append(out, t)
	}
	return gen.ListMonitorTypes200JSONResponse(out), nil
}

// monitor loads a monitor of the caller's tenant; other tenants' monitors are
// reported as not found (FA-TEN-02).
func (h *Handlers) monitor(ctx context.Context, p auth.Principal, id uuid.UUID) (store.Monitor, error) {
	m, err := h.deps.State.GetMonitor(ctx, p.TenantID, id.String())
	if errors.Is(err, store.ErrNotFound) {
		return store.Monitor{}, notFound("monitor")
	}
	return m, err
}

// monitorDocument validates the input and fills defaults (FA-MON-02/03).
// On create (creating true) an omitted notificationChannelIds assigns the
// tenant's default channels (FA-ALR-01).
func (h *Handlers) monitorDocument(ctx context.Context, tenantID string, in *gen.MonitorInput, creating bool) (cluster.MonitorDocument, error) {
	if in == nil {
		return cluster.MonitorDocument{}, &Problem{Status: http.StatusBadRequest, Title: problemTitleBadRequest, Detail: "request body is required"}
	}
	var fields []checker.FieldError
	add := func(field, msg string) { fields = append(fields, checker.FieldError{Field: field, Message: msg}) }

	doc := cluster.MonitorDocument{
		Name:              strings.TrimSpace(in.Name),
		Type:              in.Type,
		IntervalSeconds:   in.IntervalSeconds,
		Retries:           1,
		RecoveryThreshold: 1,
		Assignment:        store.Assignment{Mode: string(gen.AssignmentModeAll)},
		ConsensusPolicy:   string(gen.ConsensusPolicyMajority),
	}
	validateMonitorIdentity(in, &doc, add)
	validateMonitorTiming(in, &doc, add)

	assignment, err := h.validateMonitorAssignment(ctx, tenantID, in.Assignment, add)
	if err != nil {
		return cluster.MonitorDocument{}, err
	}
	doc.Assignment = assignment

	validateMonitorPolicyAndTags(in, &doc, add)

	channels, err := h.monitorChannels(ctx, tenantID, in.NotificationChannelIds, creating, add)
	if err != nil {
		return cluster.MonitorDocument{}, err
	}
	doc.NotificationChannelIDs = channels

	cfg, err := h.validateMonitorConfig(ctx, tenantID, in.Type, in.Config, assignment, add)
	if err != nil {
		return cluster.MonitorDocument{}, err
	}
	doc.Config = cfg

	if len(fields) > 0 {
		return cluster.MonitorDocument{}, &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed",
			Detail: "the monitor configuration is invalid", Fields: fields}
	}
	return doc, nil
}

func validateMonitorIdentity(in *gen.MonitorInput, doc *cluster.MonitorDocument, add func(string, string)) {
	if doc.Name == "" || len([]rune(doc.Name)) > maxNameLength {
		add("/name", fmt.Sprintf("must be 1 to %d characters", maxNameLength))
	}
	if in.Description != nil {
		doc.Description = *in.Description
		if len([]rune(doc.Description)) > maxDescription {
			add("/description", fmt.Sprintf("must be at most %d characters", maxDescription))
		}
	}
}

func validateMonitorTiming(in *gen.MonitorInput, doc *cluster.MonitorDocument, add func(string, string)) {
	if doc.IntervalSeconds < minIntervalSeconds || doc.IntervalSeconds > maxIntervalSeconds {
		add("/intervalSeconds", fmt.Sprintf("must be between %d and %d", minIntervalSeconds, maxIntervalSeconds))
	}
	doc.TimeoutSeconds = min(doc.IntervalSeconds*8/10, maxTimeoutSeconds)
	if in.TimeoutSeconds != nil {
		doc.TimeoutSeconds = *in.TimeoutSeconds
		if doc.TimeoutSeconds < 1 || doc.TimeoutSeconds*10 > doc.IntervalSeconds*8 {
			add("/timeoutSeconds", "must be at least 1 and at most 80 % of the interval")
		}
	}
	checkOptionalRange(in.Retries, &doc.Retries, 0, 10, "/retries", "must be between 0 and 10", add)
	checkOptionalRange(in.RecoveryThreshold, &doc.RecoveryThreshold, 1, 10, "/recoveryThreshold", "must be between 1 and 10", add)
	if in.ResendIntervalMinutes != nil {
		doc.ResendIntervalMinutes = *in.ResendIntervalMinutes
		if doc.ResendIntervalMinutes < 0 {
			add("/resendIntervalMinutes", "must not be negative")
		}
	}
}

func checkOptionalRange(val, dst *int, lo, hi int, field, msg string, add func(string, string)) {
	if val == nil {
		return
	}
	*dst = *val
	if *dst < lo || *dst > hi {
		add(field, msg)
	}
}

func (h *Handlers) validateMonitorAssignment(ctx context.Context, tenantID string, in *gen.Assignment, add func(string, string)) (store.Assignment, error) {
	a := store.Assignment{Mode: string(gen.AssignmentModeAll)}
	if in == nil {
		return a, nil
	}
	a.Mode = string(in.Mode)
	if in.Count != nil {
		a.Count = *in.Count
	}
	if in.Nodes != nil {
		a.Nodes = *in.Nodes
	}
	switch {
	case !in.Mode.Valid():
		add("/assignment/mode", "must be all, count or nodes")
	case a.Mode == string(gen.AssignmentModeCount) && a.Count < 1:
		add("/assignment/count", "required for mode count")
	case a.Mode == string(gen.AssignmentModeNodes) && len(a.Nodes) == 0:
		add("/assignment/nodes", "required for mode nodes")
	case a.Mode == string(gen.AssignmentModeNodes):
		if err := h.validateAssignedNodes(ctx, tenantID, a.Nodes, add); err != nil {
			return store.Assignment{}, err
		}
	}
	return a, nil
}

// validateAssignedNodes checks node IDs against the cluster registry (D2, ADR 0035).
// Only voters, public probes, and private probes owned by tenantID are admitted;
// foreign private probes return the same "unknown node" error as nonexistent IDs.
// Without a registry (single voter before its first registration) any node ID is
// accepted (ADR 0017).
func (h *Handlers) validateAssignedNodes(ctx context.Context, tenantID string, nodeIDs []string, add func(string, string)) error {
	registry, err := h.deps.State.ListNodes(ctx)
	if err != nil {
		return err
	}
	if len(registry) == 0 {
		return nil
	}
	known := make(map[string]bool, len(registry))
	for _, n := range registry {
		if n.Role == store.NodeRoleVoter || n.OwnerTenantID == "" || n.OwnerTenantID == tenantID {
			known[n.ID] = true
		}
	}
	for i, id := range nodeIDs {
		if !known[id] {
			add(fmt.Sprintf("/assignment/nodes/%d", i), "unknown node")
		}
	}
	return nil
}

// assignedOnlyOwnPrivateProbes reports whether the assignment explicitly targets
// only private probes owned by tenantID (ADR 0035, section 4 SSRF relaxation).
func (h *Handlers) assignedOnlyOwnPrivateProbes(ctx context.Context, tenantID string, a store.Assignment) (bool, error) {
	if a.Mode != string(gen.AssignmentModeNodes) || len(a.Nodes) == 0 {
		return false, nil
	}
	registry, err := h.deps.State.ListNodes(ctx)
	if err != nil {
		return false, err
	}
	if len(registry) == 0 {
		return false, nil
	}
	byID := make(map[string]store.Node, len(registry))
	for _, n := range registry {
		byID[n.ID] = n
	}
	for _, id := range a.Nodes {
		n, ok := byID[id]
		if !ok || n.Role != store.NodeRoleProbe || n.OwnerTenantID != tenantID {
			return false, nil
		}
	}
	return true, nil
}

func validateMonitorPolicyAndTags(in *gen.MonitorInput, doc *cluster.MonitorDocument, add func(string, string)) {
	if in.ConsensusPolicy != nil {
		doc.ConsensusPolicy = string(*in.ConsensusPolicy)
		if !in.ConsensusPolicy.Valid() {
			add("/consensusPolicy", "must be majority, any or all")
		}
	}
	validateMonitorTags(in.Tags, doc, add)
	if in.Paused != nil {
		doc.Paused = *in.Paused
	}
}

func validateMonitorTags(tags *[]string, doc *cluster.MonitorDocument, add func(string, string)) {
	if tags == nil {
		return
	}
	doc.Tags = *tags
	if len(doc.Tags) > maxTags {
		add("/tags", fmt.Sprintf("at most %d tags", maxTags))
	}
	for i, t := range doc.Tags {
		if t == "" || len([]rune(t)) > maxTagLength {
			add(fmt.Sprintf("/tags/%d", i), fmt.Sprintf("must be 1 to %d characters", maxTagLength))
		}
	}
}

func (h *Handlers) validateMonitorConfig(ctx context.Context, tenantID, monType string, rawCfg map[string]any, assignment store.Assignment, add func(string, string)) (json.RawMessage, error) {
	cfg, err := json.Marshal(rawCfg)
	if err != nil {
		return nil, err
	}
	switch err := h.deps.Registry.Validate(monType, cfg); {
	case errors.Is(err, checker.ErrUnknownType):
		add("/type", "unknown monitor type")
		return cfg, nil
	case err != nil:
		return cfg, appendValidationFields(err, add)
	}
	if err := h.checkClusterCapabilities(ctx, monType, cfg, add); err != nil {
		return nil, err
	}
	// The destination hosts are checked against the node's address policy
	// once the configuration itself is valid (ADR 0030), unless the monitor runs
	// exclusively on own private probes (ADR 0035).
	if h.deps.SSRFPolicy != nil {
		privateOnly, err := h.assignedOnlyOwnPrivateProbes(ctx, tenantID, assignment)
		if err != nil {
			return nil, err
		}
		if !privateOnly {
			if err := h.deps.Registry.ValidateTargets(monType, cfg, h.deps.SSRFPolicy); err != nil {
				if err := appendValidationFields(err, add); err != nil {
					return nil, err
				}
			}
		}
	}
	return cfg, nil
}

func appendValidationFields(err error, add func(string, string)) error {
	var verr *checker.ValidationError
	if !errors.As(err, &verr) {
		return err
	}
	for _, f := range verr.Fields {
		add("/config"+f.Field, f.Message)
	}
	return nil
}

// monitorChannels returns the notification channel IDs of a monitor. Unknown
// or foreign channels are reported through add. An omitted list takes the
// tenant's default channels on create and no channels on update; an explicit
// empty list means no channels (FA-ALR-01).
func (h *Handlers) monitorChannels(ctx context.Context, tenantID string, ids *[]openapi_types.UUID, creating bool,
	add func(field, msg string)) ([]string, error) {
	if ids == nil {
		if !creating {
			return nil, nil
		}
		all, err := h.deps.State.ListChannels(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, ch := range all {
			if ch.IsDefault {
				out = append(out, ch.ID)
			}
		}
		return out, nil
	}
	out := make([]string, 0, len(*ids))
	seen := make(map[string]bool, len(*ids))
	for _, id := range *ids {
		key := id.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		_, err := h.deps.State.GetChannel(ctx, tenantID, key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			add("/notificationChannelIds", "unknown notification channel "+key)
		case err != nil:
			return nil, err
		default:
			out = append(out, key)
		}
	}
	return out, nil
}

// apiMonitor converts one monitor and adds the newest heartbeat of any node and
// the 24 h uptime.
func (h *Handlers) apiMonitor(ctx context.Context, m store.Monitor) (gen.Monitor, error) {
	out, err := h.apiMonitors(ctx, []store.Monitor{m})
	if err != nil {
		return gen.Monitor{}, err
	}
	return out[0], nil
}

// apiMonitors converts monitors of one tenant. Each monitor gets the newest
// heartbeat of any node and its 24 h uptime. Both are loaded for all monitors
// with one query each, so a page costs a constant number of queries (NFA-PERF-03).
func (h *Handlers) apiMonitors(ctx context.Context, ms []store.Monitor) ([]gen.Monitor, error) {
	out := make([]gen.Monitor, 0, len(ms))
	if len(ms) == 0 {
		return out, nil
	}
	tenantID := ms[0].TenantID
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	last, err := h.deps.Metrics.LatestHeartbeatsByMonitor(ctx, tenantID, ids)
	if err != nil {
		return nil, err
	}
	totals, err := h.uptime.ConsensusTotals(ctx, tenantID, ms, uptime.Range24h, h.deps.Now())
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		// Stored configurations hold the mask already; masking again also covers
		// monitors written before ADR 0015 that are not migrated yet.
		paths, err := h.schemas.Paths(m.Type)
		if err != nil {
			return nil, err
		}
		if m.Config, err = configsecret.MaskConfig(paths, m.Config); err != nil {
			return nil, err
		}
		api := toAPIMonitor(m)
		if hb, ok := last[m.ID]; ok {
			at := time.UnixMilli(hb.TsMs).UTC()
			latency := hb.LatencyMs
			api.LastCheckAt = &at
			api.LastLatencyMs = &latency
		}
		if pct, ok := uptime.Percent(totals[m.ID]); ok {
			api.Uptime24h = &pct
		}
		out = append(out, api)
	}
	return out, nil
}

func toAPIMonitor(m store.Monitor) gen.Monitor {
	var cfg map[string]any
	_ = json.Unmarshal(m.Config, &cfg)
	if cfg == nil {
		cfg = map[string]any{}
	}
	assignment := gen.Assignment{Mode: gen.AssignmentMode(m.Assignment.Mode)}
	if m.Assignment.Count > 0 {
		assignment.Count = &m.Assignment.Count
	}
	if len(m.Assignment.Nodes) > 0 {
		assignment.Nodes = &m.Assignment.Nodes
	}
	policy := gen.ConsensusPolicy(m.ConsensusPolicy)
	tags := m.Tags
	flapping := m.State.Flapping
	if tags == nil {
		tags = []string{}
	}
	channelIDs := make([]openapi_types.UUID, 0, len(m.ChannelIDs))
	for _, c := range m.ChannelIDs {
		if id, err := uuid.Parse(c); err == nil {
			channelIDs = append(channelIDs, id)
		}
	}
	id, _ := uuid.Parse(m.ID)
	return gen.Monitor{
		Id:                     id,
		Name:                   m.Name,
		Description:            &m.Description,
		Type:                   m.Type,
		Config:                 cfg,
		IntervalSeconds:        m.IntervalSeconds,
		TimeoutSeconds:         &m.TimeoutSeconds,
		Retries:                &m.Retries,
		RecoveryThreshold:      &m.RecoveryThreshold,
		ResendIntervalMinutes:  &m.ResendIntervalMinutes,
		Assignment:             &assignment,
		ConsensusPolicy:        &policy,
		Tags:                   &tags,
		Paused:                 &m.Paused,
		NotificationChannelIds: &channelIDs,
		Status:                 gen.MonitorStatus(m.State.Status),
		StatusSince:            time.UnixMilli(m.State.SinceMs).UTC(),
		Flapping:               &flapping,
		CreatedAt:              time.UnixMilli(m.CreatedMs).UTC(),
		UpdatedAt:              time.UnixMilli(m.UpdatedMs).UTC(),
	}
}

func toAPIHeartbeat(hb store.Heartbeat) gen.Heartbeat {
	out := gen.Heartbeat{
		NodeId:    hb.NodeID,
		Ts:        time.UnixMilli(hb.TsMs).UTC(),
		Status:    gen.CheckStatus(checker.Status(hb.Status).String()),
		LatencyMs: hb.LatencyMs,
	}
	if hb.ErrorClass != "" {
		ec := gen.ErrorClass(hb.ErrorClass)
		out.ErrorClass = &ec
	}
	if hb.Message != "" {
		out.Message = &hb.Message
	}
	return out
}

func toAPICheckResult(res checker.Result, nodeID string, started time.Time) gen.CheckResult {
	out := gen.CheckResult{
		Status:    gen.CheckStatus(res.Status.String()),
		LatencyMs: res.LatencyMs,
		CheckedAt: started.UTC(),
		NodeId:    nodeID,
	}
	if res.Message != "" {
		out.Message = &res.Message
	}
	if res.ErrorClass != "" {
		ec := gen.ErrorClass(res.ErrorClass)
		out.ErrorClass = &ec
	}
	if len(res.Details) > 0 {
		out.Details = &res.Details
	}
	return out
}

// Cursors are opaque to clients: base64url("<kind>:<value>").
func encodeCursor(kind, value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + value))
}

func decodeCursor(cursor, kind string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	value, ok := strings.CutPrefix(string(raw), kind+":")
	if err != nil || !ok {
		return "", &Problem{Status: http.StatusBadRequest, Title: problemTitleBadRequest, Detail: "invalid cursor"}
	}
	return value, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
