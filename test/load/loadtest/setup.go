// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"crypto/sha1" //nolint:gosec // name-based UUID (RFC 4122 v5), not a security boundary
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// loadNamespace is the fixed namespace of the load-test idempotency keys.
const loadNamespace = "b1f5a7c2-3d4e-4f60-8a91-2c7e5d9b0a13"

// idempotencyKey returns the Idempotency-Key of monitor i. The key is a
// name-based UUID (version 5 layout) of prefix and index, so a rerun of setup
// sends the same keys and the voter deduplicates instead of creating twins.
func idempotencyKey(prefix string, index int) string {
	return uuidV5(parseUUID(loadNamespace), fmt.Sprintf("pulsequorum-loadtest/%s/%d", prefix, index))
}

func parseUUID(s string) [16]byte {
	var u [16]byte
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != len(u) {
		panic("invalid UUID constant: " + s)
	}
	copy(u[:], b)
	return u
}

func uuidV5(namespace [16]byte, name string) string {
	h := sha1.New()
	_, _ = h.Write(namespace[:])
	_, _ = io.WriteString(h, name)
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// monitorInput is the request body of POST /monitors (MonitorInput).
type monitorInput struct {
	Name            string         `json:"name"`
	Type            string         `json:"type"`
	Config          map[string]any `json:"config"`
	IntervalSeconds int            `json:"intervalSeconds"`
	Retries         int            `json:"retries"`
}

type setupOptions struct {
	count      int
	interval   int
	targetBase string
	prefix     string
	workers    int
}

func runSetup(ctx context.Context, args []string, stderr io.Writer, log *slog.Logger) error {
	fs := newFlagSet("setup", stderr)
	base := fs.String("base", "http://127.0.0.1:8080", "voter base URL")
	setupToken := fs.String("setup-token", "", "one-time setup token; creates the admin if the node is not set up yet")
	user := fs.String("user", "", "username (created with -setup-token, otherwise used for login)")
	password := fs.String("password", "", "password")
	opts := setupOptions{}
	fs.IntVar(&opts.count, "count", 1000, "number of monitors to create")
	fs.IntVar(&opts.interval, "interval", 60, "check interval in seconds (20..86400)")
	fs.StringVar(&opts.targetBase, "target-base", "", "target farm base URL as seen from the voter, e.g. http://host.containers.internal:18900")
	fs.StringVar(&opts.prefix, "prefix", "load-", "monitor name prefix (part of the idempotency key)")
	fs.IntVar(&opts.workers, "workers", 16, "parallel requests")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if opts.count < 1 {
		return fmt.Errorf("%w: -count must be at least 1", errUsage)
	}
	if opts.interval < 20 || opts.interval > 86400 {
		return fmt.Errorf("%w: -interval must be between 20 and 86400", errUsage)
	}
	if opts.workers < 1 {
		return fmt.Errorf("%w: -workers must be at least 1", errUsage)
	}
	if opts.targetBase == "" {
		return fmt.Errorf("%w: -target-base is required", errUsage)
	}
	opts.targetBase = strings.TrimRight(opts.targetBase, "/")
	if *user == "" || *password == "" {
		return fmt.Errorf("%w: -user and -password are required", errUsage)
	}

	client, err := newAPIClient(*base, *user, *password)
	if err != nil {
		return err
	}
	if err := authenticate(ctx, client, *setupToken, log); err != nil {
		return err
	}
	return createMonitors(ctx, client, opts, log)
}

// authenticate creates the admin with the setup token, or logs in when the
// node is already set up (409) or no setup token is given.
func authenticate(ctx context.Context, c *apiClient, setupToken string, log *slog.Logger) error {
	if setupToken != "" {
		err := c.setup(ctx, setupToken)
		if err == nil {
			log.Info("admin created", "user", c.user)
			return nil
		}
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
			return fmt.Errorf("setup: %w", err)
		}
		log.Info("node already set up, logging in")
	}
	if err := c.login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	return nil
}

func createMonitors(ctx context.Context, client *apiClient, opts setupOptions, log *slog.Logger) error {
	jobs := make(chan int)
	var done, failed atomic.Int64
	var errMu sync.Mutex
	var firstErrs []string
	var wg sync.WaitGroup

	start := time.Now()
	for w := 0; w < opts.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := createMonitor(ctx, client, opts, i); err != nil {
					failed.Add(1)
					errMu.Lock()
					if len(firstErrs) < 5 {
						firstErrs = append(firstErrs, fmt.Sprintf("index %d: %v", i, err))
					}
					errMu.Unlock()
				}
				n := done.Add(1)
				if n%100 == 0 || n == int64(opts.count) {
					log.Info("progress", "done", n, "total", opts.count, "failed", failed.Load(),
						"elapsed", time.Since(start).Round(time.Millisecond))
				}
			}
		}()
	}
feed:
	for i := 0; i < opts.count; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	elapsed := time.Since(start)
	log.Info("setup finished", "created", done.Load()-failed.Load(), "failed", failed.Load(),
		"total", opts.count, "elapsed", elapsed.Round(time.Millisecond))
	if err := ctx.Err(); err != nil {
		return err
	}
	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%d of %d monitors failed; first errors: %s", n, opts.count, strings.Join(firstErrs, "; "))
	}
	return nil
}

func createMonitor(ctx context.Context, client *apiClient, opts setupOptions, i int) error {
	body := monitorInput{
		Name:            opts.prefix + fmt.Sprintf("%04d", i),
		Type:            "http",
		Config:          map[string]any{"url": opts.targetBase + "/t/" + strconv.Itoa(i)},
		IntervalSeconds: opts.interval,
		Retries:         0,
	}
	header := http.Header{idempotencyHeader: {idempotencyKey(opts.prefix, i)}}
	_, err := client.call(ctx, http.MethodPost, "/monitors", body, header)
	return err
}
