// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/joinsider/pulsequorum/internal/store/sqlite/metricsdb"
)

// A series is one (tenant, monitor, node) of this node's metrics.db; rollups
// and latency minutes reference it by an integer ID (ADR 0016). IDs are local
// and never leave this file. Writes resolve IDs with seriesResolver; reads
// join the series table and filter by tenant in SQL, so the tenant boundary
// never depends on this cache.

type seriesKey struct{ tenant, monitor, node string }

// seriesCache holds committed series IDs. The key includes the tenant, so a
// hit can never hand out another tenant's series.
type seriesCache struct {
	mu  sync.RWMutex
	ids map[seriesKey]int64
}

func (c *seriesCache) get(k seriesKey) (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.ids[k]
	return id, ok
}

// forget drops the cached series of a tenant.
func (c *seriesCache) forget(tenantID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.ids {
		if k.tenant == tenantID {
			delete(c.ids, k)
		}
	}
}

func (c *seriesCache) add(created map[seriesKey]int64) {
	if len(created) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ids == nil {
		c.ids = make(map[seriesKey]int64, len(created))
	}
	for k, id := range created {
		c.ids[k] = id
	}
}

// seriesResolver resolves series IDs inside one write transaction. IDs it
// creates enter the cache only after the commit (writeSeries), so a rolled
// back transaction cannot leave a dangling ID behind.
type seriesResolver struct {
	cache   *seriesCache
	q       *metricsdb.Queries
	created map[seriesKey]int64
}

// id returns the series ID of (tenantID, monitorID, nodeID), creating the
// series if it does not exist. nodeID "" is the consensus view.
func (r *seriesResolver) id(ctx context.Context, tenantID, monitorID, nodeID string) (int64, error) {
	if tenantID == "" || monitorID == "" {
		return 0, errors.New("series: empty tenant or monitor ID")
	}
	k := seriesKey{tenantID, monitorID, nodeID}
	if id, ok := r.cache.get(k); ok {
		return id, nil
	}
	if id, ok := r.created[k]; ok {
		return id, nil
	}
	id, err := r.q.GetSeries(ctx, metricsdb.GetSeriesParams{TenantID: tenantID, MonitorID: monitorID, NodeID: nodeID})
	if errors.Is(err, sql.ErrNoRows) {
		id, err = r.q.InsertSeries(ctx, metricsdb.InsertSeriesParams{TenantID: tenantID, MonitorID: monitorID, NodeID: nodeID})
	}
	if err != nil {
		return 0, fmt.Errorf("series: %w", mapErr(err))
	}
	if r.created == nil {
		r.created = map[seriesKey]int64{}
	}
	r.created[k] = id
	return id, nil
}

// writeSeries runs fn in a write transaction with a series resolver and
// publishes the series it created to the cache after the commit.
func (m *MetricsStore) writeSeries(ctx context.Context, what string, fn func(q *metricsdb.Queries, series *seriesResolver) error) error {
	var res *seriesResolver
	err := m.write(ctx, what, func(q *metricsdb.Queries) error {
		res = &seriesResolver{cache: &m.series, q: q}
		return fn(q, res)
	})
	if err == nil && res != nil {
		m.series.add(res.created)
	}
	return err
}

// seriesPage is the number of series IDs read at a time by retainPerSeries.
const seriesPage = 256

// retainPerSeries runs one retention batch in one write transaction. It walks
// the series in ID order and calls del for each series with the number of rows
// the batch may still delete, so the batch never exceeds batchSize rows. Every
// statement is bounded by a primary key prefix (series_id), so no retention
// statement scans a table (FA-UPT-06, ADR 0016). It returns the rows deleted in
// this batch; a result below batchSize means every series has been visited and
// the caller can stop.
func (m *MetricsStore) retainPerSeries(ctx context.Context, what string, batchSize int,
	del func(ctx context.Context, q *metricsdb.Queries, seriesID, limit int64) (int64, error)) (int64, error) {
	if batchSize <= 0 {
		return 0, fmt.Errorf("%s: batch size must be positive", what)
	}
	var total int64
	err := m.write(ctx, what, func(q *metricsdb.Queries) error {
		total = 0
		limit := int64(batchSize)
		var after int64
		for total < limit {
			ids, err := q.ListSeriesIDsAfter(ctx, metricsdb.ListSeriesIDsAfterParams{AfterID: after, RowLimit: seriesPage})
			if err != nil {
				return mapErr(err)
			}
			if len(ids) == 0 {
				return nil
			}
			for _, id := range ids {
				n, err := del(ctx, q, id, limit-total)
				if err != nil {
					return mapErr(err)
				}
				total += n
				after = id
				if total >= limit {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
