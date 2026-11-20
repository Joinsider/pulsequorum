// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Command ssrfenv provides the network services of the local SSRF test
// environment (AS-07, ADR 0030, deploy/ssrf-dev/README.md):
//
//   - dns: authoritative DNS server for the zone ssrf.test.
//   - redirect: HTTP server that redirects to internal targets.
//   - target: HTTP canary on the "internal" network; every request is logged.
//
// It is a test helper only and never part of the product.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

func main() {
	mode := flag.String("mode", "", "service to run: dns, redirect or target")
	canaryLog := flag.String("log", "", "target mode: file that receives one line per request")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	var err error
	switch *mode {
	case "dns":
		err = serveDNS(logger)
	case "redirect":
		err = serveRedirect(logger)
	case "target":
		if *canaryLog == "" {
			fmt.Fprintln(os.Stderr, "ssrfenv: -log is required in target mode")
			os.Exit(2)
		}
		err = serveTarget(logger, *canaryLog)
	default:
		fmt.Fprintf(os.Stderr, "ssrfenv: -mode must be dns, redirect or target, got %q\n", *mode)
		os.Exit(2)
	}
	if err != nil {
		logger.Error("server stopped", "mode", *mode, "err", err)
		os.Exit(1)
	}
}
