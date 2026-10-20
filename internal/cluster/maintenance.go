// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"strings"

	"github.com/joinsider/uptimecluster/internal/store"
)

// maxMaintenanceMinutes is the longest allowed window per occurrence (one week).
const maxMaintenanceMinutes = 7 * 24 * 60

// maintenanceFromDocument validates a maintenance window document structurally
// and returns the record. Only deterministic checks run here: every replica
// must reach the same verdict, so time zones and cron expressions are not
// loaded or parsed in the FSM (the API does that before proposing).
func maintenanceFromDocument(tenantID, id string, doc MaintenanceDocument, issuedAt int64) (store.MaintenanceWindow, error) {
	if tenantID == "" {
		return store.MaintenanceWindow{}, invalid("maintenance command without tenant")
	}
	if strings.TrimSpace(doc.Name) == "" {
		return store.MaintenanceWindow{}, invalid("maintenance window without name")
	}
	s := doc.Schedule
	switch s.Kind {
	case store.ScheduleOnce:
		switch {
		case s.Cron != "":
			return store.MaintenanceWindow{}, invalid("one-time maintenance window with cron expression")
		case s.StartsMs >= s.EndsMs:
			return store.MaintenanceWindow{}, invalid("one-time maintenance window must start before it ends")
		}
	case store.ScheduleRecurring:
		switch {
		case strings.TrimSpace(s.Cron) == "":
			return store.MaintenanceWindow{}, invalid("recurring maintenance window without cron expression")
		case s.DurationMinutes < 1 || s.DurationMinutes > maxMaintenanceMinutes:
			return store.MaintenanceWindow{}, invalid("maintenance duration %d minutes out of range", s.DurationMinutes)
		case s.StartsMs != 0 && s.EndsMs != 0 && s.StartsMs >= s.EndsMs:
			return store.MaintenanceWindow{}, invalid("recurring maintenance window bounds are reversed")
		}
	default:
		return store.MaintenanceWindow{}, invalid("maintenance schedule kind %q", s.Kind)
	}
	return store.MaintenanceWindow{
		TenantID: tenantID, ID: id, Name: doc.Name, Description: doc.Description, MonitorIDs: doc.MonitorIDs,
		Schedule: s, Active: doc.Active, CreatedMs: issuedAt, UpdatedMs: issuedAt,
	}, nil
}
