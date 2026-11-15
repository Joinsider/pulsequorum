// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

// Snapshot is a normalized, comparable view of the replicated data of one
// voter. Volatile fields (statuses, timestamps of checks, last login, the
// cluster's own view of leader and health) are left out, so two voters with
// the same data produce equal snapshots.
type Snapshot struct {
	Monitors    []MonitorView     `json:"monitors"`
	Channels    []ChannelView     `json:"channels"`
	Maintenance []MaintenanceView `json:"maintenance"`
	Users       []UserView        `json:"users"`
	Members     []MemberView      `json:"members"`
}

// MonitorView is the configuration of one monitor.
type MonitorView struct {
	ID                     string            `json:"id"`
	Name                   string            `json:"name"`
	Description            string            `json:"description,omitempty"`
	Type                   string            `json:"type"`
	Config                 map[string]any    `json:"config"`
	IntervalSeconds        int               `json:"intervalSeconds"`
	TimeoutSeconds         int               `json:"timeoutSeconds"`
	Retries                int               `json:"retries"`
	RecoveryThreshold      int               `json:"recoveryThreshold"`
	ResendIntervalMinutes  int               `json:"resendIntervalMinutes"`
	Assignment             *ucapi.Assignment `json:"assignment,omitempty"`
	ConsensusPolicy        string            `json:"consensusPolicy"`
	Tags                   []string          `json:"tags,omitempty"`
	NotificationChannelIDs []string          `json:"notificationChannelIds"`
	Paused                 bool              `json:"paused"`
	CreatedAt              time.Time         `json:"createdAt"`
}

// ChannelView is the configuration of one notification channel.
type ChannelView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	MaskedURL string   `json:"maskedUrl"`
	Webhook   any      `json:"webhook,omitempty"`
	Events    []string `json:"events"`
	IsDefault bool     `json:"isDefault"`
}

// MaintenanceView is the configuration of one maintenance window.
type MaintenanceView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	MonitorIDs  []string `json:"monitorIds"`
	Schedule    any      `json:"schedule"`
	Active      bool     `json:"active"`
}

// UserView is one user of the tenant.
type UserView struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
}

// MemberView is one cluster member (without the answering node's view).
type MemberView struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Voter   bool   `json:"voter"`
	Name    string `json:"name,omitempty"`
}

// NormalizeMonitors returns the monitor configurations sorted by ID.
func NormalizeMonitors(ms []ucapi.Monitor) []MonitorView {
	out := make([]MonitorView, 0, len(ms))
	for _, m := range ms {
		out = append(out, MonitorView{
			ID:                     m.ID,
			Name:                   m.Name,
			Description:            m.Description,
			Type:                   m.Type,
			Config:                 m.Config,
			IntervalSeconds:        m.IntervalSeconds,
			TimeoutSeconds:         m.TimeoutSeconds,
			Retries:                m.Retries,
			RecoveryThreshold:      m.RecoveryThreshold,
			ResendIntervalMinutes:  m.ResendIntervalMinutes,
			Assignment:             m.Assignment,
			ConsensusPolicy:        m.ConsensusPolicy,
			Tags:                   sortedStrings(m.Tags),
			NotificationChannelIDs: sortedStrings(m.NotificationChannelIDs),
			Paused:                 m.Paused,
			CreatedAt:              m.CreatedAt.UTC(),
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// NormalizeChannels returns the channel configurations sorted by ID.
func NormalizeChannels(chs []ucapi.NotificationChannel) []ChannelView {
	out := make([]ChannelView, 0, len(chs))
	for _, ch := range chs {
		out = append(out, ChannelView{
			ID:        ch.ID,
			Name:      ch.Name,
			Kind:      ch.Kind,
			MaskedURL: ch.MaskedURL,
			Webhook:   ch.Webhook,
			Events:    sortedStrings(ch.Events),
			IsDefault: ch.IsDefault,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// NormalizeMaintenance returns the maintenance windows sorted by ID.
func NormalizeMaintenance(ws []ucapi.MaintenanceWindow) []MaintenanceView {
	out := make([]MaintenanceView, 0, len(ws))
	for _, w := range ws {
		out = append(out, MaintenanceView{
			ID:          w.ID,
			Name:        w.Name,
			Description: w.Description,
			MonitorIDs:  sortedStrings(w.MonitorIDs),
			Schedule:    w.Schedule,
			Active:      w.Active,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// NormalizeUsers returns the users sorted by ID. LastLoginAt is volatile and
// not part of the view.
func NormalizeUsers(us []ucapi.User) []UserView {
	out := make([]UserView, 0, len(us))
	for _, u := range us {
		out = append(out, UserView{
			ID:          u.ID,
			Username:    u.Username,
			DisplayName: u.DisplayName,
			Role:        u.Role,
			Disabled:    u.Disabled,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// NormalizeMembers returns the members sorted by ID. Health, replication
// lag and the answering node's flags are left out.
func NormalizeMembers(list ucapi.ClusterNodeList) []MemberView {
	out := make([]MemberView, 0, len(list.Nodes))
	for _, n := range list.Nodes {
		out = append(out, MemberView{ID: n.ID, Address: n.Address, Voter: n.Voter, Name: n.Name})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// Snapshot reads the replicated data as voter i sees it.
func (c *Cluster) Snapshot(ctx context.Context, i int) (Snapshot, error) {
	if err := c.checkIndex(i); err != nil {
		return Snapshot{}, err
	}
	cl := c.Client(i)
	monitors, err := cl.ListMonitors(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list monitors on %s: %w", c.names.Voter(i), err)
	}
	channels, err := cl.ListChannels(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list channels on %s: %w", c.names.Voter(i), err)
	}
	windows, err := cl.ListMaintenance(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list maintenance on %s: %w", c.names.Voter(i), err)
	}
	users, err := cl.ListUsers(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list users on %s: %w", c.names.Voter(i), err)
	}
	members, err := cl.ListClusterNodes(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list members on %s: %w", c.names.Voter(i), err)
	}
	return Snapshot{
		Monitors:    NormalizeMonitors(monitors),
		Channels:    NormalizeChannels(channels),
		Maintenance: NormalizeMaintenance(windows),
		Users:       NormalizeUsers(users),
		Members:     NormalizeMembers(members),
	}, nil
}

// CompareSnapshots returns the differences between two snapshots, sorted.
// An empty result means the data is identical. Entries are compared as JSON,
// so map key order does not matter.
func CompareSnapshots(a, b Snapshot) []string {
	var diffs []string
	diffs = append(diffs, compareSet("monitor",
		keyed(a.Monitors, func(m MonitorView) (string, string) { return m.ID, m.Name }),
		keyed(b.Monitors, func(m MonitorView) (string, string) { return m.ID, m.Name }))...)
	diffs = append(diffs, compareSet("channel",
		keyed(a.Channels, func(c ChannelView) (string, string) { return c.ID, c.Name }),
		keyed(b.Channels, func(c ChannelView) (string, string) { return c.ID, c.Name }))...)
	diffs = append(diffs, compareSet("maintenance window",
		keyed(a.Maintenance, func(w MaintenanceView) (string, string) { return w.ID, w.Name }),
		keyed(b.Maintenance, func(w MaintenanceView) (string, string) { return w.ID, w.Name }))...)
	diffs = append(diffs, compareSet("user",
		keyed(a.Users, func(u UserView) (string, string) { return u.ID, u.Username }),
		keyed(b.Users, func(u UserView) (string, string) { return u.ID, u.Username }))...)
	diffs = append(diffs, compareSet("member",
		keyed(a.Members, func(m MemberView) (string, string) { return m.ID, m.Address }),
		keyed(b.Members, func(m MemberView) (string, string) { return m.ID, m.Address }))...)
	sort.Strings(diffs)
	return diffs
}

// keyedEntry is one entry of a snapshot section: its JSON and a label.
type keyedEntry struct {
	label string
	json  string
}

func keyed[T any](items []T, key func(T) (string, string)) map[string]keyedEntry {
	out := make(map[string]keyedEntry, len(items))
	for _, it := range items {
		id, name := key(it)
		b, _ := json.Marshal(it)
		out[id] = keyedEntry{label: fmt.Sprintf("%s (%s)", name, id), json: string(b)}
	}
	return out
}

// compareSet reports entries missing on one side and entries that differ.
func compareSet(kind string, a, b map[string]keyedEntry) []string {
	var diffs []string
	for id, ea := range a {
		eb, ok := b[id]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s %s is only on the first voter", kind, ea.label))
			continue
		}
		if ea.json != eb.json {
			diffs = append(diffs, fmt.Sprintf("%s %s differs", kind, ea.label))
		}
	}
	for id, eb := range b {
		if _, ok := a[id]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s %s is only on the second voter", kind, eb.label))
		}
	}
	return diffs
}

func sortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
