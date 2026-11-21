// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/configsecret"
	"github.com/joinsider/pulsequorum/internal/store"
)

// checkClusterCapabilities rejects a configuration that uses fields older
// voters do not understand while any registered voter lacks the capability
// (ADR 0042, section 10). Without a registry (single voter before its first
// registration) the local binary decides, and it understands the field.
func (h *Handlers) checkClusterCapabilities(ctx context.Context, typ string, cfg json.RawMessage, add func(field, msg string)) error {
	need := h.deps.Registry.ClusterCapabilities(typ, cfg)
	if len(need) == 0 {
		return nil
	}
	nodes, err := h.deps.State.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	var missing []string
	for _, n := range nodes {
		if n.Role != store.NodeRoleVoter {
			continue
		}
		for _, c := range need {
			if !slices.Contains(n.Capabilities, c) {
				missing = append(missing, n.Name)
				break
			}
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		add("/config", fmt.Sprintf("cluster upgrade in progress: voters %s do not support this configuration yet; upgrade them first",
			strings.Join(slices.Compact(missing), ", ")))
	}
	return nil
}

// validateResolved validates the configuration of doc again with its stored
// secrets put back. On PUT a masked secret keeps the stored value, so checks
// that need the secret itself (e.g. that a private key matches its
// certificate, ADR 0042 section 3) only run here. The plaintext stays in
// memory; field errors name pointers only.
func (h *Handlers) validateResolved(ctx context.Context, doc cluster.MonitorDocument, monitorID string) error {
	if len(doc.Secrets) == 0 {
		return nil
	}
	cfg, err := configsecret.Resolve(ctx, h.keyring(), monitorID, doc.Config, doc.Secrets)
	if err != nil {
		return fmt.Errorf("resolve secrets of monitor %s: %w", monitorID, err)
	}
	err = h.deps.Registry.Validate(doc.Type, cfg)
	var verr *checker.ValidationError
	switch {
	case err == nil:
		return nil
	case !errors.As(err, &verr):
		return err
	}
	fields := make([]checker.FieldError, 0, len(verr.Fields))
	for _, f := range verr.Fields {
		fields = append(fields, checker.FieldError{Field: "/config" + f.Field, Message: f.Message})
	}
	return &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed", Detail: "the monitor configuration is invalid", Fields: fields}
}
