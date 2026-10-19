// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package api implements the REST API defined in api/openapi.yaml on top of the
// generated strict server (ADR 0003).
package api

// Generated code (gen/, unimplemented.go) is refreshed with `make generate`.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/config"
	"github.com/joinsider/uptimecluster/internal/version"
)

// BaseURL is the path prefix of all REST endpoints.
const BaseURL = "/api/v1"

// ErrNotImplemented marks operations that exist in the contract but not yet in code.
var ErrNotImplemented = errors.New("not implemented")

// Handlers implements gen.StrictServerInterface.
type Handlers struct {
	Unimplemented
	cfg config.Config
}

var _ gen.StrictServerInterface = (*Handlers)(nil)

// Register mounts all API operations on mux under BaseURL.
func Register(mux *http.ServeMux, cfg config.Config, log *slog.Logger) {
	strict := gen.NewStrictHandlerWithOptions(&Handlers{cfg: cfg}, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			WriteProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, ErrNotImplemented) {
				WriteProblem(w, http.StatusNotImplemented, "Not Implemented", r.Method+" "+r.URL.Path+" is not implemented yet")
				return
			}
			log.Error("api handler failed", "method", r.Method, "path", r.URL.Path, "err", err)
			WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "")
		},
	})
	gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{
		BaseURL:    BaseURL,
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			WriteProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		},
	})
}

// GetHealth returns the public node status.
func (h *Handlers) GetHealth(context.Context, gen.GetHealthRequestObject) (gen.GetHealthResponseObject, error) {
	return gen.GetHealth200JSONResponse{
		Status:   gen.HealthStatusOk,
		Role:     gen.HealthRole(h.cfg.Role),
		NodeName: h.cfg.NodeName,
		Version:  version.Version,
		Commit:   version.Commit,
	}, nil
}

// WriteProblem writes an RFC 9457 problem details response.
func WriteProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(gen.Problem{Type: "about:blank", Title: title, Status: status, Detail: &detail})
}
