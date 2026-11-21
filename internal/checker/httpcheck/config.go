// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package httpcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/joinsider/pulsequorum/internal/checker"
)

// Defaults (FA-CHK-10, FA-CHK-12, FA-CHK-14).
const (
	defaultExpectedStatus = "200-299"
	defaultMaxRedirects   = 10
	defaultWarningDays    = 14
	defaultMaxBodyBytes   = 1 << 20
	maxMaxBodyBytes       = 16 << 20
)

// Config is the HTTP(S) monitor configuration. Its JSON form is described by
// schemaJSON; both must change together.
type Config struct {
	URL                     string            `json:"url"`
	Method                  string            `json:"method,omitempty"`
	Headers                 map[string]string `json:"headers,omitempty"`
	Body                    string            `json:"body,omitempty"`
	Auth                    *Auth             `json:"auth,omitempty"`
	ExpectedStatus          string            `json:"expectedStatus,omitempty"`
	Keyword                 *Keyword          `json:"keyword,omitempty"`
	JSONPath                *JSONPathCheck    `json:"jsonPath,omitempty"`
	MaxResponseMs           int               `json:"maxResponseMs,omitempty"`
	FollowRedirects         *bool             `json:"followRedirects,omitempty"`
	MaxRedirects            int               `json:"maxRedirects,omitempty"`
	IgnoreTLSErrors         bool              `json:"ignoreTlsErrors,omitempty"`
	TLSExpiryWarningDays    *int              `json:"tlsExpiryWarningDays,omitempty"`
	IPFamily                string            `json:"ipFamily,omitempty"`
	MaxBodyBytes            int               `json:"maxBodyBytes,omitempty"`
	ConnectTimeoutMs        int               `json:"connectTimeoutMs,omitempty"`
	TLSTimeoutMs            int               `json:"tlsTimeoutMs,omitempty"`
	ResponseHeaderTimeoutMs int               `json:"responseHeaderTimeoutMs,omitempty"`
	UserAgent               string            `json:"userAgent,omitempty"`
	// ClientCertificate is presented when the server asks for one (ADR 0042).
	ClientCertificate *ClientCertificate `json:"clientCertificate,omitempty"`
}

// Auth adds Basic or Bearer authentication.
type Auth struct {
	Type     string `json:"type"` // basic | bearer
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
}

// Keyword checks the presence or absence of a text in the body.
type Keyword struct {
	Value         string `json:"value"`
	Mode          string `json:"mode,omitempty"` // contains (default) | notContains
	CaseSensitive bool   `json:"caseSensitive,omitempty"`
}

// JSONPathCheck compares the value at a path with an expected string.
type JSONPathCheck struct {
	Path     string `json:"path"`
	Expected string `json:"expected"`
}

const schemaJSON = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "HTTP(S)",
  "type": "object",
  "additionalProperties": false,
  "required": ["url"],
  "properties": {
    "url": {"type": "string", "title": "URL", "format": "uri", "maxLength": 2048},
    "method": {"type": "string", "enum": ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"], "default": "GET"},
    "headers": {"type": "object", "additionalProperties": {"type": "string", "maxLength": 4096, "writeOnly": true}, "maxProperties": 50, "description": "Values are stored encrypted and shown masked"},
    "body": {"type": "string", "maxLength": 65536},
    "auth": {
      "type": "object",
      "additionalProperties": false,
      "required": ["type"],
      "properties": {
        "type": {"type": "string", "enum": ["basic", "bearer"]},
        "username": {"type": "string"},
        "password": {"type": "string", "writeOnly": true},
        "token": {"type": "string", "writeOnly": true}
      }
    },
    "expectedStatus": {"type": "string", "default": "200-299", "description": "Codes and ranges, e.g. 200-299,301"},
    "keyword": {
      "type": "object",
      "additionalProperties": false,
      "required": ["value"],
      "properties": {
        "value": {"type": "string", "minLength": 1, "maxLength": 1024},
        "mode": {"type": "string", "enum": ["contains", "notContains"], "default": "contains"},
        "caseSensitive": {"type": "boolean", "default": false}
      }
    },
    "jsonPath": {
      "type": "object",
      "additionalProperties": false,
      "required": ["path", "expected"],
      "properties": {
        "path": {"type": "string", "minLength": 1, "maxLength": 512, "description": "e.g. $.status or $.items[0].state"},
        "expected": {"type": "string", "maxLength": 1024}
      }
    },
    "maxResponseMs": {"type": "integer", "minimum": 1},
    "followRedirects": {"type": "boolean", "default": true},
    "maxRedirects": {"type": "integer", "minimum": 0, "maximum": 30, "default": 10},
    "ignoreTlsErrors": {"type": "boolean", "default": false},
    "tlsExpiryWarningDays": {"type": "integer", "minimum": 0, "maximum": 365, "default": 14},
    "ipFamily": {"type": "string", "enum": ["auto", "ipv4", "ipv6"], "default": "auto"},
    "maxBodyBytes": {"type": "integer", "minimum": 1, "maximum": 16777216, "default": 1048576},
    "connectTimeoutMs": {"type": "integer", "minimum": 1},
    "tlsTimeoutMs": {"type": "integer", "minimum": 1},
    "responseHeaderTimeoutMs": {"type": "integer", "minimum": 1},
    "userAgent": {"type": "string", "maxLength": 256},
    "clientCertificate": {
      "type": "object",
      "title": "Client certificate (mTLS)",
      "description": "Sent only when the server asks for one, and only to the host of the URL",
      "additionalProperties": false,
      "required": ["certificate", "privateKey"],
      "properties": {
        "certificate": {"type": "string", "title": "Certificate (PEM)", "minLength": 1, "maxLength": 16384, "description": "Leaf certificate first, optionally followed by intermediates"},
        "privateKey": {"type": "string", "title": "Private key (PEM)", "minLength": 1, "maxLength": 16384, "writeOnly": true, "description": "Unencrypted PKCS#8, PKCS#1 or SEC 1; stored encrypted and shown masked"}
      }
    }
  }
}`

// parse decodes cfg and fills defaults.
func parse(raw json.RawMessage) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode configuration: %w", err)
	}
	if c.Method == "" {
		c.Method = "GET"
	}
	if c.ExpectedStatus == "" {
		c.ExpectedStatus = defaultExpectedStatus
	}
	if c.FollowRedirects == nil {
		follow := true
		c.FollowRedirects = &follow
	}
	if c.MaxRedirects == 0 {
		c.MaxRedirects = defaultMaxRedirects
	}
	if c.TLSExpiryWarningDays == nil {
		days := defaultWarningDays
		c.TLSExpiryWarningDays = &days
	}
	if c.IPFamily == "" {
		c.IPFamily = "auto"
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	c.MaxBodyBytes = min(c.MaxBodyBytes, maxMaxBodyBytes)
	return &c, nil
}

// numericHost matches hosts that look like IP addresses in decimal, octal or
// hexadecimal notation (e.g. 2852039166, 0177.0.0.1, 0x7f000001).
var numericHost = regexp.MustCompile(`(?i)^(?:[0-9.]+|[0-9a-fx.]*0x[0-9a-fx.]*)$`)

func validate(raw json.RawMessage) error {
	c, err := parse(raw)
	if err != nil {
		return err
	}
	var fields []checker.FieldError
	add := func(field, msg string) { fields = append(fields, checker.FieldError{Field: field, Message: msg}) }

	u, err := url.Parse(c.URL)
	switch {
	case err != nil:
		add("/url", "not a valid URL")
	case u.Scheme != "http" && u.Scheme != "https":
		add("/url", "scheme must be http or https")
	case u.Hostname() == "":
		add("/url", "host is missing")
	case u.User != nil:
		add("/url", "credentials in the URL are not allowed; use auth")
	default:
		host := u.Hostname()
		if _, perr := netip.ParseAddr(host); perr != nil && numericHost.MatchString(host) {
			add("/url", "non-canonical IP address notation is not allowed")
		}
	}

	if _, err := parseStatusRanges(c.ExpectedStatus); err != nil {
		add("/expectedStatus", err.Error())
	}
	if c.JSONPath != nil {
		if _, err := parsePath(c.JSONPath.Path); err != nil {
			add("/jsonPath/path", err.Error())
		}
	}
	if c.Auth != nil {
		switch {
		case c.Auth.Type == "basic" && c.Auth.Username == "":
			add("/auth/username", "required for basic auth")
		case c.Auth.Type == "bearer" && c.Auth.Token == "":
			add("/auth/token", "required for bearer auth")
		}
	}
	if c.ClientCertificate != nil {
		validateClientCertificate(c.ClientCertificate, add)
	}
	for name, value := range c.Headers {
		if !validHeaderName(name) || strings.ContainsAny(value, "\r\n") {
			add("/headers/"+name, "invalid header")
		}
	}
	if (c.Method == http.MethodGet || c.Method == http.MethodHead) && c.Body != "" {
		add("/body", "GET and HEAD requests cannot have a body")
	}
	if c.Method == http.MethodHead && (c.Keyword != nil || c.JSONPath != nil) {
		add("/method", "HEAD responses have no body to check")
	}
	if len(fields) > 0 {
		return &checker.ValidationError{Fields: fields}
	}
	return nil
}

// Targets implements checker.TargetLister: the host of the URL is the only
// destination the check connects to (ADR 0030).
func (*Checker) Targets(raw json.RawMessage) ([]checker.Target, error) {
	c, err := parse(raw)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("not a valid URL: %w", err)
	}
	return []checker.Target{{Field: "/url", Host: u.Hostname()}}, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r) {
			return false
		}
	}
	return true
}

type statusRange struct{ lo, hi int }

// parseStatusRanges parses "200-299,301".
func parseStatusRanges(s string) ([]statusRange, error) {
	var out []statusRange
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("invalid status code %q", part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("invalid status range %q", part)
			}
		}
		if a < 100 || b > 599 || a > b {
			return nil, fmt.Errorf("status range %q out of 100-599", part)
		}
		out = append(out, statusRange{a, b})
	}
	if len(out) == 0 {
		return nil, errors.New("no status codes given")
	}
	return out, nil
}

func statusMatches(ranges []statusRange, code int) bool {
	for _, r := range ranges {
		if code >= r.lo && code <= r.hi {
			return true
		}
	}
	return false
}
