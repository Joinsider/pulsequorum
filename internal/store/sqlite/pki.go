// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

// ListClusterCAs returns the cluster CAs ordered by creation.
func (s *StateStore) ListClusterCAs(ctx context.Context) ([]store.ClusterCA, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListClusterCAs(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.ClusterCA, 0, len(rows))
	for _, r := range rows {
		out = append(out, clusterCAFromRow(r))
	}
	return out, nil
}

// GetJoinToken returns the join token with the ID.
func (s *StateStore) GetJoinToken(ctx context.Context, id string) (store.JoinToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getJoinToken(ctx, s.q, id)
}

// ListJoinTokens returns all join tokens ordered by creation and ID.
func (s *StateStore) ListJoinTokens(ctx context.Context) ([]store.JoinToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListJoinTokens(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.JoinToken, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.JoinToken{
			ID: r.ID, SecretHash: r.SecretHash, Role: r.Role, TenantID: r.TenantID,
			ExpiresMs: r.ExpiresMs, CreatedMs: r.CreatedMs, UsedMs: r.UsedMs, UsedBy: r.UsedBy,
		})
	}
	return out, nil
}

// ListRevokedCerts returns the revoked certificates ordered by serial.
func (s *StateStore) ListRevokedCerts(ctx context.Context) ([]store.RevokedCert, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListRevokedCerts(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.RevokedCert, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.RevokedCert{
			Serial: r.Serial, NodeID: r.NodeID, RevokedMs: r.RevokedMs, NotAfterMs: r.NotAfterMs,
		})
	}
	return out, nil
}

// ListRevokedNodes returns the revoked nodes ordered by node ID.
func (s *StateStore) ListRevokedNodes(ctx context.Context) ([]store.RevokedNode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.q.ListRevokedNodes(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.RevokedNode, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.RevokedNode{NodeID: r.NodeID, RevokedMs: r.RevokedMs})
	}
	return out, nil
}

func (t *stateTx) ListClusterCAs(ctx context.Context) ([]store.ClusterCA, error) {
	rows, err := t.q.ListClusterCAs(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.ClusterCA, 0, len(rows))
	for _, r := range rows {
		out = append(out, clusterCAFromRow(r))
	}
	return out, nil
}

func (t *stateTx) GetJoinToken(ctx context.Context, id string) (store.JoinToken, error) {
	return getJoinToken(ctx, t.q, id)
}

func (t *stateTx) PutClusterCA(ctx context.Context, ca store.ClusterCA) error {
	if ca.Fingerprint == "" || ca.ClusterID == "" || len(ca.CertDER) == 0 || ca.KeyEnc == "" {
		return errors.New("put cluster CA: fingerprint, cluster ID, certificate and encrypted key are required")
	}
	return mapErr(t.q.InsertClusterCA(ctx, statedb.InsertClusterCAParams{
		Fingerprint: ca.Fingerprint, ClusterID: ca.ClusterID, CertDer: ca.CertDER,
		KeyEnc: ca.KeyEnc, CreatedMs: ca.CreatedMs,
	}))
}

func (t *stateTx) InsertJoinToken(ctx context.Context, jt store.JoinToken) error {
	if jt.ID == "" || jt.SecretHash == "" || jt.Role == "" {
		return errors.New("insert join token: ID, secret hash and role are required")
	}
	return mapErr(t.q.InsertJoinToken(ctx, statedb.InsertJoinTokenParams{
		ID: jt.ID, SecretHash: jt.SecretHash, Role: jt.Role, TenantID: jt.TenantID,
		ExpiresMs: jt.ExpiresMs, CreatedMs: jt.CreatedMs,
	}))
}

func (t *stateTx) MarkJoinTokenUsed(ctx context.Context, id string, usedMs int64, usedBy string) error {
	n, err := t.q.MarkJoinTokenUsed(ctx, statedb.MarkJoinTokenUsedParams{UsedMs: sql.NullInt64{Int64: usedMs, Valid: true}, UsedBy: usedBy, ID: id})
	if err != nil {
		return mapErr(err)
	}
	if n > 0 {
		return nil
	}
	// Either the token is missing or it was used before (the first use is kept).
	if _, err := getJoinToken(ctx, t.q, id); err != nil {
		return err
	}
	return store.ErrConflict
}

func (t *stateTx) DeleteJoinToken(ctx context.Context, id string) error {
	n, err := t.q.DeleteJoinToken(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (t *stateTx) PruneJoinTokens(ctx context.Context, beforeMs int64) error {
	return mapErr(t.q.PruneJoinTokens(ctx, beforeMs))
}

func (t *stateTx) PutRevokedCert(ctx context.Context, c store.RevokedCert) error {
	if c.Serial == "" || c.NodeID == "" {
		return errors.New("put revoked certificate: serial and node ID are required")
	}
	return mapErr(t.q.PutRevokedCert(ctx, statedb.PutRevokedCertParams{
		Serial: c.Serial, NodeID: c.NodeID, RevokedMs: c.RevokedMs, NotAfterMs: c.NotAfterMs,
	}))
}

func (t *stateTx) PruneRevokedCerts(ctx context.Context, beforeMs int64) error {
	return mapErr(t.q.PruneRevokedCerts(ctx, beforeMs))
}

func (t *stateTx) PutRevokedNode(ctx context.Context, n store.RevokedNode) error {
	if n.NodeID == "" {
		return errors.New("put revoked node: node ID is required")
	}
	return mapErr(t.q.PutRevokedNode(ctx, statedb.PutRevokedNodeParams{NodeID: n.NodeID, RevokedMs: n.RevokedMs}))
}

func getJoinToken(ctx context.Context, q *statedb.Queries, id string) (store.JoinToken, error) {
	r, err := q.GetJoinToken(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.JoinToken{}, store.ErrNotFound
	}
	if err != nil {
		return store.JoinToken{}, mapErr(err)
	}
	return store.JoinToken{
		ID: r.ID, SecretHash: r.SecretHash, Role: r.Role, TenantID: r.TenantID,
		ExpiresMs: r.ExpiresMs, CreatedMs: r.CreatedMs, UsedMs: r.UsedMs, UsedBy: r.UsedBy,
	}, nil
}

func clusterCAFromRow(r statedb.ClusterCa) store.ClusterCA {
	return store.ClusterCA{
		Fingerprint: r.Fingerprint, ClusterID: r.ClusterID, CertDER: r.CertDer,
		KeyEnc: r.KeyEnc, CreatedMs: r.CreatedMs,
	}
}
