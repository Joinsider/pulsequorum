// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/configsecret"
)

// This file holds the secret handling of the monitor API (ADR 0015): values
// at writeOnly positions of a type's schema are encrypted before proposing
// and never returned; plaintext exists only in the request and while a check
// runs.

// errNoKeyring is returned when a secret must be encrypted but the node has
// no keyring (only in tests or misconfigured setups).
var errNoKeyring = errors.New("secret encryption is not configured")

type noKeyring struct{}

func (noKeyring) Encrypt([]byte, string) (string, error) { return "", errNoKeyring }

func (noKeyring) Decrypt(context.Context, string, string) ([]byte, error) { return nil, errNoKeyring }

func (h *Handlers) keyring() configsecret.Keyring {
	if h.deps.Keyring == nil {
		return noKeyring{}
	}
	return h.deps.Keyring
}

// sealSecrets moves the secrets of doc.Config into doc.Secrets, encrypted for
// monitorID. previous are the stored secrets of the same monitor and type;
// the mask keeps them (same semantics as notification channels).
func (h *Handlers) sealSecrets(doc cluster.MonitorDocument, monitorID string, previous configsecret.Secrets) (cluster.MonitorDocument, error) {
	paths, err := h.schemas.Paths(doc.Type)
	if err != nil {
		return cluster.MonitorDocument{}, err
	}
	config, sec, err := configsecret.Split(h.keyring(), paths, monitorID, doc.Config, previous)
	var maskErr *configsecret.MaskError
	if errors.As(err, &maskErr) {
		return cluster.MonitorDocument{}, &Problem{Status: http.StatusUnprocessableEntity, Title: "Validation Failed",
			Detail: "the monitor configuration is invalid", Fields: []checker.FieldError{{
				Field: "/config" + maskErr.Pointer, Message: "a masked value can only keep a stored secret; enter the value",
			}}}
	}
	if err != nil {
		return cluster.MonitorDocument{}, err
	}
	doc.Config, doc.Secrets = config, sec
	return doc, nil
}
