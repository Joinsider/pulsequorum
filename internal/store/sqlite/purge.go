// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"errors"
	"fmt"

	"github.com/joinsider/pulsequorum/internal/store/sqlite/metricsdb"
)

// PurgeTenant deletes up to batchSize rows of the tenant from each metrics
// table that holds tenant data: heartbeats and degraded alerts by tenant_id,
// rollups and latency minutes through their series, and the series last, once
// nothing references them (ADR 0029). It returns the rows deleted in this call;
// 0 means nothing of the tenant is left. One call is one write transaction.
func (m *MetricsStore) PurgeTenant(ctx context.Context, tenantID string, batchSize int) (int64, error) {
	if tenantID == "" {
		return 0, errors.New("purge tenant: empty tenant ID")
	}
	if batchSize <= 0 {
		return 0, errors.New("purge tenant: batch size must be positive")
	}
	limit := int64(batchSize)
	var total int64
	err := m.write(ctx, "purge tenant", func(q *metricsdb.Queries) error {
		var err error
		total, err = purgeTenantTx(ctx, q, tenantID, limit)
		return err
	})
	if err != nil {
		return 0, err
	}
	// Cached series IDs of the tenant may point at deleted rows; drop them.
	m.series.forget(tenantID)
	return total, nil
}

func purgeTenantTx(ctx context.Context, q *metricsdb.Queries, tenantID string, limit int64) (int64, error) {
	var total int64
	n, err := q.PurgeHeartbeatsOfTenant(ctx, metricsdb.PurgeHeartbeatsOfTenantParams{TenantID: tenantID, RowLimit: limit})
	if err != nil {
		return 0, mapErr(err)
	}
	total += n
	n, err = q.PurgeDegradedAlertsOfTenant(ctx, metricsdb.PurgeDegradedAlertsOfTenantParams{TenantID: tenantID, RowLimit: limit})
	if err != nil {
		return 0, mapErr(err)
	}
	total += n
	n, err = purgeTenantSeriesData(ctx, q, tenantID, limit)
	if err != nil {
		return 0, err
	}
	total += n
	n, err = q.PurgeSeriesOfTenant(ctx, metricsdb.PurgeSeriesOfTenantParams{TenantID: tenantID, RowLimit: limit})
	if err != nil {
		return 0, mapErr(err)
	}
	return total + n, nil
}

func purgeTenantSeriesData(ctx context.Context, q *metricsdb.Queries, tenantID string, limit int64) (int64, error) {
	rollupsLeft, minutesLeft := limit, limit
	var total, after int64
	for rollupsLeft > 0 || minutesLeft > 0 {
		ids, err := q.ListSeriesIDsOfTenantAfter(ctx, metricsdb.ListSeriesIDsOfTenantAfterParams{
			TenantID: tenantID, AfterID: after, RowLimit: seriesPage,
		})
		if err != nil {
			return 0, mapErr(err)
		}
		if len(ids) == 0 {
			break
		}
		n, lastID, err := purgeSeriesBatch(ctx, q, ids, &rollupsLeft, &minutesLeft)
		if err != nil {
			return 0, err
		}
		total += n
		after = lastID
	}
	return total, nil
}

func purgeSeriesBatch(ctx context.Context, q *metricsdb.Queries, ids []int64, rollupsLeft, minutesLeft *int64) (int64, int64, error) {
	var total, after int64
	for _, id := range ids {
		after = id
		n, err := purgeOneSeries(ctx, q, id, rollupsLeft, minutesLeft)
		if err != nil {
			return 0, 0, err
		}
		total += n
		if *rollupsLeft <= 0 && *minutesLeft <= 0 {
			break
		}
	}
	return total, after, nil
}

func purgeOneSeries(ctx context.Context, q *metricsdb.Queries, seriesID int64, rollupsLeft, minutesLeft *int64) (int64, error) {
	var total int64
	if *rollupsLeft > 0 {
		n, err := q.PurgeRollupsOfSeries(ctx, metricsdb.PurgeRollupsOfSeriesParams{SeriesID: seriesID, RowLimit: *rollupsLeft})
		if err != nil {
			return 0, mapErr(err)
		}
		total += n
		*rollupsLeft -= n
	}
	if *minutesLeft > 0 {
		n, err := q.PurgeMinutesOfSeries(ctx, metricsdb.PurgeMinutesOfSeriesParams{SeriesID: seriesID, RowLimit: *minutesLeft})
		if err != nil {
			return 0, mapErr(err)
		}
		total += n
		*minutesLeft -= n
	}
	return total, nil
}

// PurgedTenants returns the IDs of the tenants whose purge finished on this
// node, ordered by ID.
func (m *MetricsStore) PurgedTenants(ctx context.Context) ([]string, error) {
	ids, err := m.q.ListPurgedTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list purged tenants: %w", mapErr(err))
	}
	return ids, nil
}

// MarkTenantPurged records that the purge of the tenant finished on this node.
func (m *MetricsStore) MarkTenantPurged(ctx context.Context, tenantID string, purgedMs int64) error {
	if tenantID == "" {
		return errors.New("mark tenant purged: empty tenant ID")
	}
	return mapErr(metricsdb.New(m.db.writer).UpsertPurgedTenant(ctx, metricsdb.UpsertPurgedTenantParams{
		TenantID: tenantID, PurgedMs: purgedMs,
	}))
}
