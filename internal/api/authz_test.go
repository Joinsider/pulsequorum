// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// accessLevel is the minimum role level an operation needs (FA-AUTH-09).
type accessLevel int

const (
	needRead     accessLevel = iota // any authenticated role
	needEdit                        // editor and above
	needAdmin                       // tenant-admin and above
	needPlatform                    // platform-admin only (cluster administration)
)

// roleLevel orders the roles; anonymous callers are handled separately.
var roleLevel = map[string]accessLevel{
	"tenant-viewer":  needRead,
	"tenant-editor":  needEdit,
	"tenant-admin":   needAdmin,
	"platform-admin": needPlatform,
}

// TestPermissionMatrix checks every role against representative operations of
// each area with real local tokens (FA-AUTH-09). Anonymous callers get 401;
// roles below the required level get 403; everything else must get a status
// that is neither 401 nor 403. Operations on unknown IDs and invalid bodies
// keep the checks free of side effects.
func TestPermissionMatrix(t *testing.T) {
	e, admin := newLocalAdminEnv(t)
	_, tenantAdmin := e.addUser(admin, "matrix-tadmin", "tenant-admin")
	_, editor := e.addUser(admin, "matrix-editor", "tenant-editor")
	_, viewer := e.addUser(admin, "matrix-viewer", "tenant-viewer")

	tokens := map[string]string{
		"tenant-viewer":  viewer,
		"tenant-editor":  editor,
		"tenant-admin":   tenantAdmin,
		"platform-admin": admin,
	}

	missing := uuid.NewString()
	type op struct {
		name   string
		method string
		path   string
		body   any
		need   accessLevel
	}
	ops := []op{
		// Reads: any role.
		{"me", "GET", "/api/v1/me", nil, needRead},
		{"change own password (wrong current)", "PUT", "/api/v1/me/password",
			map[string]any{"currentPassword": "not the current one", "newPassword": "another long password"}, needRead},
		{"list monitors", "GET", "/api/v1/monitors", nil, needRead},
		{"monitor types", "GET", "/api/v1/monitor-types", nil, needRead},
		{"get monitor", "GET", "/api/v1/monitors/" + missing, nil, needRead},
		{"monitor heartbeats", "GET", "/api/v1/monitors/" + missing + "/heartbeats", nil, needRead},
		{"monitor uptime", "GET", "/api/v1/monitors/" + missing + "/uptime?range=24h", nil, needRead},
		{"list incidents", "GET", "/api/v1/incidents", nil, needRead},
		{"list notification channels", "GET", "/api/v1/notifications", nil, needRead},
		{"list maintenance windows", "GET", "/api/v1/maintenance", nil, needRead},

		// Monitor and maintenance writes: editor and above.
		{"create monitor", "POST", "/api/v1/monitors", httpMonitor("matrix", "https://example.com"), needEdit},
		{"update monitor", "PUT", "/api/v1/monitors/" + missing, httpMonitor("matrix", "https://example.com"), needEdit},
		{"pause monitor", "POST", "/api/v1/monitors/" + missing + "/pause", nil, needEdit},
		{"test monitor", "POST", "/api/v1/monitors/" + missing + "/test", nil, needEdit},
		{"delete monitor", "DELETE", "/api/v1/monitors/" + missing, nil, needEdit},
		{"create maintenance window", "POST", "/api/v1/maintenance", map[string]any{}, needEdit},
		{"update maintenance window", "PUT", "/api/v1/maintenance/" + missing, map[string]any{}, needEdit},
		{"delete maintenance window", "DELETE", "/api/v1/maintenance/" + missing, nil, needEdit},

		// Notification channels and user management: tenant-admin and above.
		{"create notification channel", "POST", "/api/v1/notifications", map[string]any{}, needAdmin},
		{"update notification channel", "PUT", "/api/v1/notifications/" + missing, map[string]any{}, needAdmin},
		{"delete notification channel", "DELETE", "/api/v1/notifications/" + missing, nil, needAdmin},
		{"test notification channel", "POST", "/api/v1/notifications/" + missing + "/test", nil, needAdmin},
		{"list users", "GET", "/api/v1/users", nil, needAdmin},
		{"create user", "POST", "/api/v1/users",
			map[string]any{"username": "matrix-created", "password": testPassword, "role": "tenant-viewer"}, needAdmin},
		{"update user", "PUT", "/api/v1/users/" + missing, map[string]any{"displayName": "x"}, needAdmin},
		{"delete user", "DELETE", "/api/v1/users/" + missing, nil, needAdmin},

		// Cluster administration: platform admins only (FA-CLU-08). The
		// matrix env has no cluster host, so platform admins get 503.
		{"create join token", "POST", "/api/v1/admin/join-tokens", map[string]any{"role": "probe"}, needPlatform},
		{"list cluster nodes", "GET", "/api/v1/admin/nodes", nil, needPlatform},
		{"remove cluster node", "DELETE", "/api/v1/admin/nodes/" + missing, nil, needPlatform},

		// System channels and system alerts: platform admins only (ADR 0025).
		{"list system channels", "GET", "/api/v1/admin/system-channels", nil, needPlatform},
		{"create system channel", "POST", "/api/v1/admin/system-channels", map[string]any{}, needPlatform},
		{"get system channel", "GET", "/api/v1/admin/system-channels/" + missing, nil, needPlatform},
		{"update system channel", "PUT", "/api/v1/admin/system-channels/" + missing, map[string]any{}, needPlatform},
		{"delete system channel", "DELETE", "/api/v1/admin/system-channels/" + missing, nil, needPlatform},
		{"test system channel", "POST", "/api/v1/admin/system-channels/" + missing + "/test", nil, needPlatform},
		{"list system alerts", "GET", "/api/v1/admin/system-alerts", nil, needPlatform},
	}

	for _, o := range ops {
		// Anonymous callers never get past authentication.
		if res := e.do(o.method, o.path, "", o.body); res.code != http.StatusUnauthorized {
			t.Errorf("%s: anonymous = %d, want 401", o.name, res.code)
		}
		for role, token := range tokens {
			res := e.do(o.method, o.path, "", o.body, bearer(token)...)
			forbidden := roleLevel[role] < o.need
			switch {
			case res.code == http.StatusUnauthorized:
				t.Errorf("%s: %s = 401 with a valid token: %s", o.name, role, res.body)
			case forbidden && res.code != http.StatusForbidden:
				t.Errorf("%s: %s = %d, want 403: %s", o.name, role, res.code, res.body)
			case !forbidden && res.code == http.StatusForbidden:
				t.Errorf("%s: %s = 403, want allowed: %s", o.name, role, res.body)
			}
		}
	}
}
