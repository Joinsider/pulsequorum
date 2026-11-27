// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/joinsider/pulsequorum/internal/frontendcfg"
	"github.com/joinsider/pulsequorum/internal/frontendrun"
	"github.com/joinsider/pulsequorum/internal/webfront"
)

const frontendUsage = `Usage:
  pulsequorum frontend                          serve the SPA as a standalone frontend host
  pulsequorum frontend headers [--format F]     print the security headers; F is netlify (default) or nginx

Environment:
  FRONTEND_NODES     required: comma-separated voter origins, e.g. https://n1.example.com,https://n2.example.com
  AUTH_MODE          local (default) or oidc
  OIDC_ISSUER        oidc only, required: issuer URL
  OIDC_CLIENT_ID     oidc only, required: client ID of the SPA
  OIDC_SCOPES        oidc only, optional: comma or space separated, must contain openid
                     (default: openid profile offline_access)
  FRONTEND_LISTEN    listen address (default :8080)
  FRONTEND_TLS_CERT  TLS certificate file, set together with FRONTEND_TLS_KEY
  FRONTEND_TLS_KEY   TLS private key file
  FRONTEND_HSTS      auto (default: on if TLS is configured), on or off
`

// runFrontend implements the frontend subcommand. getenv supplies the
// configuration, the output goes to stdout and stderr, and ctx controls the
// lifetime of the server.
func runFrontend(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			_, _ = fmt.Fprint(stdout, frontendUsage)
			return 0
		case "headers":
			return runFrontendHeaders(args[1:], getenv, stdout, stderr)
		}
	}

	cfg, err := loadFrontendConfig(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "frontend: unexpected argument %q\n\n%s", args[0], frontendUsage)
		return 2
	}

	log := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := frontendrun.Run(ctx, cfg, log); err != nil {
		_, _ = fmt.Fprintf(stderr, "frontend: %v\n", err)
		return 1
	}
	return 0
}

// runFrontendHeaders prints the security headers of the SPA in the format of
// a static host (netlify, the _headers file) or as nginx include.
func runFrontendHeaders(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pulsequorum frontend headers", flag.ContinueOnError)
	flags.SetOutput(stderr)
	format := flags.String("format", "netlify", "output format: netlify (_headers file) or nginx (include file)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "frontend headers: unexpected argument %q\n", flags.Arg(0))
		return 2
	}

	var render func([]webfront.Header) (string, error)
	switch *format {
	case "netlify":
		render = webfront.HeadersFile
	case "nginx":
		render = webfront.NginxConf
	default:
		_, _ = fmt.Fprintf(stderr, "frontend headers: unknown format %q (want netlify or nginx)\n", *format)
		return 2
	}

	cfg, err := loadFrontendConfig(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	out, err := render(cfg.Headers())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	if _, err := fmt.Fprint(stdout, out); err != nil {
		_, _ = fmt.Fprintf(stderr, "frontend headers: %v\n", err)
		return 1
	}
	return 0
}

// loadFrontendConfig adapts getenv to the lookup with default used by frontendcfg.
func loadFrontendConfig(getenv func(string) string) (frontendcfg.Config, error) {
	return frontendcfg.Load(func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	})
}
