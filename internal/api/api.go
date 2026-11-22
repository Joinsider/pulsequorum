// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package api implements the REST API defined in api/openapi.yaml on top of the
// generated strict server (ADR 0003).
package api

// Generated code (gen/, unimplemented.go) is refreshed with `make generate`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/configsecret"
	"github.com/joinsider/pulsequorum/internal/degraded"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/replication"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/stream"
	"github.com/joinsider/pulsequorum/internal/uptime"
	"github.com/joinsider/pulsequorum/internal/version"
)

// BaseURL is the path prefix of all REST endpoints.
const BaseURL = "/api/v1"

// ErrNotImplemented marks operations that exist in the contract but not yet in code.
var ErrNotImplemented = errors.New("not implemented")

// Applier proposes commands to the Raft cluster.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// ProbeSessionView describes a live probe session for the cluster listing (ADR 0034).
type ProbeSessionView struct {
	ConnectedVoterID string
	Buffer           *pb.ProbeBufferStatus
}

// Deps are the services the handlers use. Fields may be nil in tests that do
// not exercise the corresponding endpoints.
type Deps struct {
	State   store.State
	Metrics store.Metrics
	Cluster Applier
	// ClusterAdmin serves the cluster administration (FA-CLU-08); nil answers 503.
	ClusterAdmin ClusterAdmin
	// Replication reports the state of the heartbeat replication to the other
	// voters for the cluster listing (FA-REP-05); nil omits the lag.
	Replication interface {
		Status() []replication.PeerStatus
	}
	// ClockOffsets returns the fresh clock offsets of the peers in
	// milliseconds, by node ID (ADR 0020); nil omits them.
	ClockOffsets func() map[string]int64
	// SSRFProtectionByNode returns whether SSRF protection is on for each node
	// that answered recently, including this node (ADR 0030). Nil means unknown.
	SSRFProtectionByNode func() map[string]bool
	// ProbeSessions returns merged active probe sessions by probe ID (ADR 0034).
	ProbeSessions func() map[string]ProbeSessionView
	Registry      *checker.Registry
	// SSRFPolicy checks the destination hosts of saved monitors; nil when
	// SSRF protection is off (ADR 0030).
	SSRFPolicy checker.AddressPolicy
	// CheckEnv runs on-demand checks (POST /monitors/{id}/test).
	CheckEnv *checker.Env
	NodeID   string
	// Authenticator verifies bearer credentials; nil denies every protected
	// operation (auth.DenyAll).
	Authenticator auth.Authenticator
	// Now is the clock for uptime ranges; nil = time.Now.
	Now func() time.Time
	// Keyring encrypts and decrypts channel secrets (ADR 0011, 0014).
	Keyring alert.Keyring
	// Alerts validates channels and sends test messages.
	Alerts *alert.Sender
	// Local is the local authentication (AUTH_MODE=local); nil otherwise.
	Local *auth.Local
	// Stream and Tickets serve the live WebSocket stream; nil disables it.
	Stream  *stream.Hub
	Tickets *auth.Tickets
	// StreamClosing is closed when the server shuts down; open streams end then.
	StreamClosing <-chan struct{}
	// Degraded reports the degraded state of this voter (ADR 0024) for the
	// health endpoint; nil omits it.
	Degraded interface{ State() degraded.State }
}

// Handlers implements gen.StrictServerInterface.
type Handlers struct {
	Unimplemented
	cfg    config.Config
	deps   Deps
	log    *slog.Logger
	uptime *uptime.Service
	// schemas caches the secret paths of each monitor type (ADR 0015).
	schemas *configsecret.Schemas
}

var _ gen.StrictServerInterface = (*Handlers)(nil)

// Problem is an error that is sent as RFC 9457 problem details.
type Problem struct {
	Status     int
	Title      string
	Detail     string
	Fields     []checker.FieldError
	RetryAfter int // seconds; 0 = no header
}

const (
	problemTitleBadRequest          = "Bad Request"
	problemTitleValidationFailed    = "Validation Failed"
	problemTitleUnprocessableEntity = "Unprocessable Entity"
	problemTitleServiceUnavailable  = "Service Unavailable"
)

func (p *Problem) Error() string { return fmt.Sprintf("%d %s: %s", p.Status, p.Title, p.Detail) }

// Register mounts all API operations on mux under BaseURL. It panics if the
// embedded OpenAPI spec cannot be read, which is a build defect.
func Register(mux *http.ServeMux, cfg config.Config, log *slog.Logger, deps Deps) {
	if deps.Authenticator == nil {
		deps.Authenticator = auth.DenyAll{}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	public, err := publicOperations()
	if err != nil {
		panic(err)
	}
	tokenOps, err := tokenOperations()
	if err != nil {
		panic(err)
	}
	h := &Handlers{cfg: cfg, deps: deps, log: log, uptime: &uptime.Service{State: deps.State, Metrics: deps.Metrics},
		schemas: &configsecret.Schemas{Registry: deps.Registry}}
	strict := gen.NewStrictHandlerWithOptions(h, []gen.StrictMiddlewareFunc{authenticate(public, tokenOps, deps.Authenticator)}, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			WriteProblem(w, http.StatusBadRequest, problemTitleBadRequest, err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			var p *Problem
			switch {
			case errors.As(err, &p):
				writeProblem(w, p)
			case errors.Is(err, ErrNotImplemented):
				WriteProblem(w, http.StatusNotImplemented, "Not Implemented", r.Method+" "+r.URL.Path+" is not implemented yet")
			default:
				log.Error("api handler failed", "method", r.Method, "path", r.URL.Path, "err", err)
				WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "")
			}
		},
	})
	// The WebSocket upgrade is not part of the OpenAPI contract.
	mux.HandleFunc("GET "+streamPath, h.serveStream)
	gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{
		BaseURL:    BaseURL,
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			WriteProblem(w, http.StatusBadRequest, problemTitleBadRequest, err.Error())
		},
	})
}

// GetHealth returns the public node status.
func (h *Handlers) GetHealth(context.Context, gen.GetHealthRequestObject) (gen.GetHealthResponseObject, error) {
	out := gen.GetHealth200JSONResponse{
		Status:   gen.HealthStatusOk,
		Role:     gen.HealthRole(h.cfg.Role),
		NodeName: h.cfg.NodeName,
		Version:  version.Version,
		Commit:   version.Commit,
	}
	if h.deps.Degraded != nil {
		if st := h.deps.Degraded.State(); st.Degraded {
			since := st.Since.UTC()
			partition := st.Partition
			out.Degraded = true
			out.DegradedSince = &since
			out.Partition = &partition
		}
	}
	return out, nil
}

// WriteProblem writes an RFC 9457 problem details response.
func WriteProblem(w http.ResponseWriter, status int, title, detail string) {
	writeProblem(w, &Problem{Status: status, Title: title, Detail: detail})
}

func writeProblem(w http.ResponseWriter, p *Problem) {
	body := gen.Problem{Type: "about:blank", Title: p.Title, Status: p.Status}
	if p.Detail != "" {
		body.Detail = &p.Detail
	}
	if len(p.Fields) > 0 {
		fields := make([]gen.FieldError, 0, len(p.Fields))
		for _, f := range p.Fields {
			fields = append(fields, gen.FieldError{Field: f.Field, Message: f.Message})
		}
		body.Errors = &fields
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	if p.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(p.RetryAfter))
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(body)
}
