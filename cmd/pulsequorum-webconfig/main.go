// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Command pulsequorum-webconfig writes the runtime files of the nginx frontend
// image before nginx starts: config.json and the include file with the
// security headers (ADR 0036, 6B). It is configured with the same environment
// variables as `pulsequorum frontend` and embeds no SPA.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/joinsider/pulsequorum/internal/frontendcfg"
	"github.com/joinsider/pulsequorum/internal/version"
	"github.com/joinsider/pulsequorum/internal/webfront"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run returns the exit code. Usage errors exit with 2; a configuration or
// write error exits with 1, so that the container does not start with an
// invalid configuration.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "version" {
		fmt.Fprintf(stdout, "pulsequorum-webconfig %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		return 0
	}

	flags := flag.NewFlagSet("pulsequorum-webconfig", flag.ContinueOnError)
	flags.SetOutput(stderr)
	webRoot := flags.String("web-root", "", "directory that receives config.json (required)")
	headersFile := flags.String("headers", "", "file that receives the nginx include with the security headers (required)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *webRoot == "" || *headersFile == "" {
		fmt.Fprintln(stderr, "--web-root and --headers are required")
		flags.Usage()
		return 2
	}

	cfg, err := frontendcfg.Load(func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	})
	if err != nil {
		fmt.Fprintf(stderr, "invalid configuration:\n%v\n", err)
		return 1
	}
	configJSON, err := cfg.ConfigJSON()
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	nginxConf, err := webfront.NginxConf(cfg.Headers())
	if err != nil {
		fmt.Fprintf(stderr, "invalid configuration:\n%v\n", err)
		return 1
	}

	if err := writeFileAtomic(filepath.Join(*webRoot, "config.json"), configJSON); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if err := writeFileAtomic(*headersFile, []byte(nginxConf)); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

// writeFileAtomic replaces path with data through a temporary file in the same
// directory, so that nginx never reads a partial file. The mode is 0600.
func writeFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	// The web image runs nginx master and workers as the same UID as this
	// helper (101), so owner-only access is enough.
	if err = os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", name, path, err)
	}
	return nil
}
