// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package webui serves the embedded Angular single-page application.
package webui

import (
	"embed"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// dist is filled by `make build` (see ADR 0001). The committed .gitkeep keeps the
// pattern valid when the frontend has not been built.
//
//go:embed all:dist
var dist embed.FS

// hashedAsset matches file names produced with Angular's output hashing,
// e.g. main-4K2ZQ3XH.js or chunk-ABCDEFGH.js.
var hashedAsset = regexp.MustCompile(`-[A-Z0-9]{8}\.[a-z0-9]+$`)

// Handler serves the embedded SPA.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory exists
	}
	return NewHandler(sub)
}

// NewHandler serves an SPA from fsys: existing files are served directly, any other
// path without a file extension falls back to index.html for client-side routing.
func NewHandler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if isHidden(name) {
			http.NotFound(w, r)
			return
		}

		if name != "" && name != "index.html" {
			if serveFile(w, r, fsys, name) {
				return
			}
			// Paths that look like files must not fall back to index.html,
			// otherwise missing assets would be answered with HTML.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}
		if !serveFile(w, r, fsys, "index.html") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "UptimeCluster UI is not built. Run `make build`.\n")
		}
	})
}

// serveFile writes name from fsys and reports whether it existed.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}

	w.Header().Set("Cache-Control", cacheControl(name))
	http.ServeContent(w, r, name, time.Time{}, rs)
	return true
}

func cacheControl(name string) string {
	switch {
	case name == "index.html":
		return "no-cache"
	case hashedAsset.MatchString(path.Base(name)):
		return "public, max-age=31536000, immutable"
	default:
		return "public, max-age=3600"
	}
}

func isHidden(name string) bool {
	for part := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// Built reports whether the embedded SPA contains an index.html.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return !errors.Is(err, fs.ErrNotExist)
}
