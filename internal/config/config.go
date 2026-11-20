// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package config loads and validates the node configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/joinsider/pulsequorum/internal/selfcheck"
	"github.com/joinsider/pulsequorum/internal/tenant"
)

// Role is the role a node runs in.
type Role string

// Supported node roles: a voter participates in the Raft cluster, a probe only
// runs checks.
const (
	RoleVoter Role = "voter"
	RoleProbe Role = "probe"
)

// AuthMode selects how users authenticate.
type AuthMode string

// Supported authentication modes. AuthZitadel is the legacy value of
// AUTH_MODE=zitadel. Load maps it to AuthOIDC with the zitadel profile and
// never returns it; it stays for callers that still reference it.
const (
	AuthLocal   AuthMode = "local"
	AuthOIDC    AuthMode = "oidc"
	AuthZitadel AuthMode = "zitadel"
)

// OIDCProfile selects where roles and tenants come from in an OIDC token (ADR 0027).
type OIDCProfile string

// Supported OIDC profiles.
const (
	ProfileGeneric OIDCProfile = "generic"
	ProfileZitadel OIDCProfile = "zitadel"
)

// Tenancy selects how the zitadel profile maps tokens to tenants (ADR 0027).
type Tenancy string

// Supported tenancy modes of the zitadel profile.
const (
	TenancySingle Tenancy = "single"
	TenancyMulti  Tenancy = "multi"
)

// Supported role sources of the generic profile (ADR 0028).
const (
	// RolesSourceAccessToken reads the roles claim from the access token (default).
	RolesSourceAccessToken = "access_token"
	// RolesSourceUserInfo reads the roles claim from the userinfo endpoint.
	RolesSourceUserInfo = "userinfo"
)

// OIDCConfig is the OpenID Connect configuration (ADR 0027). It is zero unless
// AuthMode is AuthOIDC. Secrets do not occur here; the client IDs are public.
type OIDCConfig struct {
	// Profile is generic or zitadel (OIDC_PROFILE).
	Profile OIDCProfile
	// Issuer is the exact iss claim and discovery base, kept as given (OIDC_ISSUER).
	Issuer string
	// Audience must be contained in the aud claim (OIDC_AUDIENCE). With the
	// zitadel profile it is the project ID.
	Audience string
	// ClientIDs are the accepted azp values; the first one is the SPA client (OIDC_CLIENT_IDS).
	ClientIDs []string
	// RequireAZP rejects tokens without azp when true (OIDC_REQUIRE_AZP).
	RequireAZP bool
	// Algorithms are the accepted signature algorithms, each RS256, ES256 or EdDSA (OIDC_ALGORITHMS).
	Algorithms []string
	// MaxTokenTTL is the largest allowed exp minus iat (OIDC_MAX_TOKEN_TTL).
	MaxTokenTTL time.Duration
	// Scopes are requested from the identity provider (OIDC_SCOPES). They
	// always contain openid when set explicitly.
	Scopes []string
	// InsecureHTTP allows an http issuer, for development only (OIDC_INSECURE_HTTP).
	InsecureHTTP bool
	// RolesClaim is a dot path to the roles claim, generic profile only (OIDC_ROLES_CLAIM).
	RolesClaim string
	// RoleMap maps identity provider values to roles, generic profile only (OIDC_ROLE_MAP).
	RoleMap map[string]string
	// RolesSource is access_token or userinfo, generic profile only (OIDC_ROLES_SOURCE).
	RolesSource string
	// DefaultRole is the role for a valid token without a role from the roles source,
	// empty or a tenant role, generic profile only (OIDC_DEFAULT_ROLE).
	DefaultRole string
	// Tenancy is single or multi, zitadel profile only (OIDC_TENANCY). The generic profile is always single.
	Tenancy Tenancy
	// AutoProvision creates the tenant of an unknown organization on its first
	// login (OIDC_AUTO_PROVISION). Only with the zitadel profile and multi tenancy (ADR 0029).
	AutoProvision bool
	// AutoProvisionPlan is the plan of automatically created tenants (OIDC_AUTO_PROVISION_PLAN).
	AutoProvisionPlan string
	// AutoProvisionMax is the highest number of automatically created tenants,
	// counted cluster-wide (OIDC_AUTO_PROVISION_MAX).
	AutoProvisionMax int
	// PlatformOrgID is the Zitadel organization of the platform, zitadel profile only (ZITADEL_PLATFORM_ORG_ID).
	PlatformOrgID string
}

// Config is the validated node configuration.
type Config struct {
	NodeName    string
	Role        Role
	DataDir     string
	HTTPAddr    string
	ClusterAddr string
	// ClusterAdvertise is the stable host:port other nodes use to reach this
	// node; it becomes the Raft server address (ADR 0005, FA-CLU-10).
	ClusterAdvertise string
	// ClusterJoin lists host:port of existing voters to join (FA-CLU-03). A
	// node with existing Raft state ignores it; without state and without
	// ClusterJoin it bootstraps a new cluster (FA-CLU-02).
	ClusterJoin []string
	// JoinToken or JoinTokenFile holds the one-time join token (FA-PKI-02).
	// Never log it.
	JoinToken     string
	JoinTokenFile string
	// RaftTimeout is the Raft heartbeat and election timeout (ADR 0012 A6).
	RaftTimeout time.Duration
	PublicURL   string
	// CORSOrigins are the origins (scheme://host[:port]) of external frontends
	// allowed to call the API from a browser (FA-API-05). The origin of
	// PublicURL is always included.
	CORSOrigins []string
	// MasterKey (base64) or MasterKeyFile provide the KEK (ADR 0011). Never log them.
	MasterKey     string
	MasterKeyFile string
	// ICMPMode is auto, privileged, unprivileged or off (FA-CHK-31).
	ICMPMode string
	// SSRFProtection is auto, on or off (FA-SEC-SSRF); auto enables it in
	// multi-tenant (Zitadel) mode.
	SSRFProtection string
	AuthMode       AuthMode
	// AuthModeAlias is set when AUTH_MODE=zitadel was given. The mode is then
	// AuthOIDC with the zitadel profile; main logs a notice (ADR 0027).
	AuthModeAlias bool
	// OIDC is the OpenID Connect configuration (ADR 0027), zero unless AuthMode is AuthOIDC.
	OIDC OIDCConfig
	// RawCompaction compacts heartbeats older than 48 h (FA-UPT-04).
	RawCompaction bool
	// Retention of raw data, hourly and daily rollups (FA-UPT-05); 0 = forever.
	RetentionRaw    time.Duration
	RetentionHourly time.Duration
	RetentionDaily  time.Duration
	// AuditRetention is how long audit log entries are kept, in command time
	// (AUDIT_RETENTION, FA-DAT-01, ADR 0031). At least 24 hours.
	AuditRetention time.Duration
	// AuthCookieDomain is the Domain of the refresh cookie; empty = host-only cookie (ADR 0008).
	AuthCookieDomain string
	// AlertMaxAttempts is the number of delivery attempts per alert and channel (FA-ALR-03).
	AlertMaxAttempts int
	// AlertBundleWindow bundles the alerts of one Shoutrrr channel that fall due
	// within this window into one message; 0 disables bundling (ADR 0023).
	AlertBundleWindow time.Duration
	// SelfCheckTargets are the raw, trimmed SELF_CHECK_TARGETS entries (ADR 0022);
	// empty disables the connectivity self-check. Parse them with selfcheck.ParseTargets.
	SelfCheckTargets []string
	// SelfHealthFailRatio is the share of deviating heartbeats at which a node
	// counts as impaired, in (0, 1] (ADR 0022).
	SelfHealthFailRatio float64
	// DegradedAfter is how long a voter may know no leader before it enters the
	// degraded mode (DEGRADED_AFTER, ADR 0024). Zero disables the degraded
	// mode, so nothing is sent without quorum.
	DegradedAfter time.Duration
	// MetricsAddr is the listen address of the Prometheus endpoint, host:port
	// (FA-OBS-02, ADR 0025). Empty disables it.
	MetricsAddr string
	// MetricsToken or MetricsTokenFile protect /metrics with a bearer token.
	// Never log them. Read the token with ResolveMetricsToken.
	MetricsToken     string
	MetricsTokenFile string
	// ReplicationLagAlert is the replication lag from which a system alert is
	// raised (REPLICATION_LAG_ALERT, FA-REP-05, ADR 0025).
	ReplicationLagAlert time.Duration
	// ClockSkewAlert is the clock offset from which a system alert is raised
	// (CLOCK_SKEW_ALERT, FA-OBS-01, ADR 0020, ADR 0025).
	ClockSkewAlert time.Duration
	// DiskFreeAlert is the free share of the data directory below which a
	// system alert is raised (DISK_FREE_ALERT, FA-OBS-01, ADR 0025), in (0, 1).
	DiskFreeAlert float64
	// WALSizeAlert is the size in bytes of a WAL file above which a system
	// alert is raised (WAL_SIZE_ALERT, FA-OBS-01, ADR 0025).
	WALSizeAlert    int64
	LogLevel        slog.Level
	LogFormat       string
	ShutdownTimeout time.Duration
}

// Load reads the configuration using getenv (usually os.Getenv) and validates it.
// All validation errors are reported at once.
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	hostname, _ := os.Hostname()
	cfg := Config{
		NodeName:    get("NODE_NAME", hostname),
		Role:        Role(get("NODE_ROLE", string(RoleVoter))),
		DataDir:     get("DATA_DIR", "/data"),
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		ClusterAddr: get("CLUSTER_ADDR", ":9443"),
		PublicURL:   strings.TrimRight(get("PUBLIC_URL", ""), "/"),
		AuthMode:    AuthMode(get("AUTH_MODE", string(AuthLocal))),
		LogFormat:   get("LOG_FORMAT", "json"),
	}

	var errs []error
	switch cfg.Role {
	case RoleVoter, RoleProbe:
	default:
		errs = append(errs, fmt.Errorf("NODE_ROLE: must be %q or %q, got %q", RoleVoter, RoleProbe, cfg.Role))
	}

	switch cfg.AuthMode {
	case AuthLocal, AuthOIDC:
	case AuthZitadel:
		// Alias: AUTH_MODE=zitadel is oidc with the zitadel profile (ADR 0027).
		cfg.AuthMode = AuthOIDC
		cfg.AuthModeAlias = true
	default:
		errs = append(errs, fmt.Errorf("AUTH_MODE: must be %q, %q or %q, got %q", AuthLocal, AuthOIDC, AuthZitadel, cfg.AuthMode))
	}
	if cfg.AuthMode == AuthOIDC {
		oidcCfg, oidcErrs := loadOIDC(get, cfg.AuthModeAlias)
		cfg.OIDC = oidcCfg
		errs = append(errs, oidcErrs...)
	}

	if cfg.NodeName == "" {
		errs = append(errs, errors.New("NODE_NAME: must not be empty"))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	switch cfg.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT: must be \"json\" or \"text\", got %q", cfg.LogFormat))
	}

	if cfg.PublicURL != "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("PUBLIC_URL: must be an absolute http(s) URL, got %q", cfg.PublicURL))
		}
	}

	if cfg.PublicURL != "" {
		if o, err := ParseOrigin(cfg.PublicURL); err == nil {
			cfg.CORSOrigins = append(cfg.CORSOrigins, o)
		}
	}
	for _, raw := range strings.Split(get("CORS_ORIGINS", ""), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		o, err := ParseOrigin(raw)
		if u, _ := url.Parse(raw); err == nil && u.Path != "" && u.Path != "/" {
			err = fmt.Errorf("must be an origin without path, got %q", raw)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("CORS_ORIGINS: %w", err))
			continue
		}
		if !slices.Contains(cfg.CORSOrigins, o) {
			cfg.CORSOrigins = append(cfg.CORSOrigins, o)
		}
	}

	cfg.MasterKey = getenv("MASTER_KEY")
	cfg.MasterKeyFile = get("MASTER_KEY_FILE", "")
	if cfg.MasterKey != "" && cfg.MasterKeyFile != "" {
		errs = append(errs, errors.New("MASTER_KEY and MASTER_KEY_FILE: set only one"))
	}

	cfg.ICMPMode = get("ICMP_MODE", "auto")
	switch cfg.ICMPMode {
	case "auto", "privileged", "unprivileged", "off":
	default:
		errs = append(errs, fmt.Errorf("ICMP_MODE: must be auto, privileged, unprivileged or off, got %q", cfg.ICMPMode))
	}

	cfg.SSRFProtection = get("SSRF_PROTECTION", "auto")
	switch cfg.SSRFProtection {
	case "auto", "on", "off":
	default:
		errs = append(errs, fmt.Errorf("SSRF_PROTECTION: must be auto, on or off, got %q", cfg.SSRFProtection))
	}

	cfg.ClusterAdvertise = get("CLUSTER_ADVERTISE", "")
	if cfg.ClusterAdvertise == "" {
		_, port, _ := net.SplitHostPort(cfg.ClusterAddr)
		cfg.ClusterAdvertise = net.JoinHostPort(cfg.NodeName, port)
	}
	if host, port, err := net.SplitHostPort(cfg.ClusterAdvertise); err != nil || host == "" || port == "" {
		errs = append(errs, fmt.Errorf("CLUSTER_ADVERTISE: must be host:port, got %q", cfg.ClusterAdvertise))
	}

	for _, seed := range strings.Split(getenv("CLUSTER_JOIN"), ",") {
		seed = strings.TrimSpace(seed)
		if seed == "" {
			continue
		}
		if host, port, err := net.SplitHostPort(seed); err != nil || host == "" || port == "" {
			errs = append(errs, fmt.Errorf("CLUSTER_JOIN: entries must be host:port, got %q", seed))
			continue
		}
		cfg.ClusterJoin = append(cfg.ClusterJoin, seed)
	}
	cfg.JoinToken = getenv("JOIN_TOKEN")
	cfg.JoinTokenFile = getenv("JOIN_TOKEN_FILE")
	if cfg.JoinToken != "" && cfg.JoinTokenFile != "" {
		errs = append(errs, errors.New("set either JOIN_TOKEN or JOIN_TOKEN_FILE, not both"))
	}

	raftTimeout, err := time.ParseDuration(get("RAFT_TIMEOUT", "1s"))
	if err != nil || raftTimeout < 100*time.Millisecond {
		errs = append(errs, fmt.Errorf("RAFT_TIMEOUT: must be a duration >= 100ms, got %q", getenv("RAFT_TIMEOUT")))
	}
	cfg.RaftTimeout = raftTimeout

	switch v := get("RAW_COMPACTION", "true"); v {
	case "true", "false":
		cfg.RawCompaction = v == "true"
	default:
		errs = append(errs, fmt.Errorf("RAW_COMPACTION: must be true or false, got %q", v))
	}
	days := func(key, def string, minDays int) time.Duration {
		v := get(key, def)
		n, err := strconv.Atoi(v)
		if err != nil || n < minDays {
			errs = append(errs, fmt.Errorf("%s: must be a whole number of days >= %d, got %q", key, minDays, v))
			return 0
		}
		return time.Duration(n) * 24 * time.Hour
	}
	// Raw data must outlive compaction and the rollup of the last day.
	cfg.RetentionRaw = days("RETENTION_RAW_DAYS", "30", 3)
	cfg.RetentionHourly = days("RETENTION_HOURLY_DAYS", "365", 1)
	cfg.RetentionDaily = days("RETENTION_DAILY_DAYS", "0", 0)

	auditRaw := get("AUDIT_RETENTION", "8760h")
	auditRetention, err := time.ParseDuration(auditRaw)
	if err != nil || auditRetention < 24*time.Hour {
		errs = append(errs, fmt.Errorf("AUDIT_RETENTION: must be a duration of at least 24h, got %q", auditRaw))
	}
	cfg.AuditRetention = auditRetention

	cfg.AuthCookieDomain = get("AUTH_COOKIE_DOMAIN", "")
	if strings.ContainsAny(cfg.AuthCookieDomain, " ;,/:") {
		errs = append(errs, fmt.Errorf("AUTH_COOKIE_DOMAIN: must be a domain name, got %q", cfg.AuthCookieDomain))
	}

	attemptsRaw := get("ALERT_MAX_ATTEMPTS", "10")
	attempts, err := strconv.Atoi(attemptsRaw)
	if err != nil || attempts < 1 || attempts > 100 {
		errs = append(errs, fmt.Errorf("ALERT_MAX_ATTEMPTS: must be a whole number from 1 to 100, got %q", attemptsRaw))
	}
	cfg.AlertMaxAttempts = attempts

	bundleRaw := get("ALERT_BUNDLE_WINDOW", "30s")
	bundleWindow, err := time.ParseDuration(bundleRaw)
	if err != nil || bundleWindow < 0 {
		errs = append(errs, fmt.Errorf("ALERT_BUNDLE_WINDOW: must be a duration of 0 or more, got %q", bundleRaw))
	}
	cfg.AlertBundleWindow = bundleWindow

	for _, raw := range strings.Split(getenv("SELF_CHECK_TARGETS"), ",") {
		if raw = strings.TrimSpace(raw); raw != "" {
			cfg.SelfCheckTargets = append(cfg.SelfCheckTargets, raw)
		}
	}
	if _, err := selfcheck.ParseTargets(cfg.SelfCheckTargets); err != nil {
		errs = append(errs, fmt.Errorf("SELF_CHECK_TARGETS: %w", err))
	}

	ratioRaw := get("SELF_HEALTH_FAIL_RATIO", "0.5")
	ratio, err := strconv.ParseFloat(ratioRaw, 64)
	if err != nil || !(ratio > 0 && ratio <= 1) {
		errs = append(errs, fmt.Errorf("SELF_HEALTH_FAIL_RATIO: must be a number greater than 0 and at most 1, got %q", ratioRaw))
	}
	cfg.SelfHealthFailRatio = ratio

	degradedRaw := get("DEGRADED_AFTER", "15s")
	degradedAfter, err := time.ParseDuration(degradedRaw)
	if err != nil || degradedAfter < 0 {
		errs = append(errs, fmt.Errorf("DEGRADED_AFTER: must be a duration of 0 or more, got %q", degradedRaw))
	}
	cfg.DegradedAfter = degradedAfter

	cfg.MetricsAddr = get("METRICS_ADDR", "")
	if cfg.MetricsAddr != "" {
		_, port, err := net.SplitHostPort(cfg.MetricsAddr)
		if n, perr := strconv.Atoi(port); err != nil || perr != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("METRICS_ADDR: must be host:port with a port from 1 to 65535, got %q", cfg.MetricsAddr))
		}
	}
	cfg.MetricsToken = strings.TrimSpace(getenv("METRICS_TOKEN"))
	cfg.MetricsTokenFile = get("METRICS_TOKEN_FILE", "")
	if cfg.MetricsToken != "" && cfg.MetricsTokenFile != "" {
		errs = append(errs, errors.New("METRICS_TOKEN and METRICS_TOKEN_FILE: set only one"))
	}

	cfg.ReplicationLagAlert, err = durationAbove(get("REPLICATION_LAG_ALERT", "5m"), 0)
	if err != nil {
		errs = append(errs, fmt.Errorf("REPLICATION_LAG_ALERT: must be a duration above 0, got %q", getenv("REPLICATION_LAG_ALERT")))
	}
	cfg.ClockSkewAlert, err = durationAbove(get("CLOCK_SKEW_ALERT", "2s"), 0)
	if err != nil {
		errs = append(errs, fmt.Errorf("CLOCK_SKEW_ALERT: must be a duration above 0, got %q", getenv("CLOCK_SKEW_ALERT")))
	}
	diskRaw := get("DISK_FREE_ALERT", "0.15")
	disk, err := strconv.ParseFloat(diskRaw, 64)
	if err != nil || !(disk > 0 && disk < 1) {
		errs = append(errs, fmt.Errorf("DISK_FREE_ALERT: must be a number greater than 0 and less than 1, got %q", diskRaw))
	}
	cfg.DiskFreeAlert = disk
	walRaw := get("WAL_SIZE_ALERT", "512MiB")
	wal, err := parseByteSize(walRaw)
	if err != nil || wal <= 0 {
		errs = append(errs, fmt.Errorf("WAL_SIZE_ALERT: must be a size above 0 such as 512MiB (B, KiB, MiB or GiB), got %q", walRaw))
	}
	cfg.WALSizeAlert = wal

	timeout, err := time.ParseDuration(get("SHUTDOWN_TIMEOUT", "10s"))
	if err != nil || timeout <= 0 {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: must be a positive duration, got %q", getenv("SHUTDOWN_TIMEOUT")))
	}
	cfg.ShutdownTimeout = timeout

	return cfg, errors.Join(errs...)
}

// durationAbove parses a duration that must be greater than floor.
func durationAbove(raw string, floor time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d <= floor {
		return 0, fmt.Errorf("duration %v is not above %v", d, floor)
	}
	return d, nil
}

// byteUnits are the suffixes of parseByteSize, longest first so that "MiB" is
// not read as "B".
var byteUnits = []struct {
	suffix string
	factor int64
}{
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

// parseByteSize parses a whole number of bytes with an optional binary unit,
// such as "512MiB". A number without a unit counts bytes.
func parseByteSize(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	factor := int64(1)
	for _, u := range byteUnits {
		if strings.HasSuffix(s, u.suffix) {
			s, factor = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.factor
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/factor {
		return 0, fmt.Errorf("invalid byte size %q", raw)
	}
	return n * factor, nil
}

// ResolveMetricsToken returns the bearer token of /metrics. A token file is
// read and trimmed; an empty result means the endpoint is not protected by a
// token. The returned value is a secret and must not be logged.
func (c Config) ResolveMetricsToken() (string, error) {
	if c.MetricsTokenFile == "" {
		return c.MetricsToken, nil
	}
	raw, err := os.ReadFile(c.MetricsTokenFile)
	if err != nil {
		return "", fmt.Errorf("METRICS_TOKEN_FILE: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("METRICS_TOKEN_FILE: file is empty")
	}
	return token, nil
}

// ParseOrigin returns the canonical origin (lower-case scheme://host[:port])
// of an absolute http(s) URL. URLs with a path, query or user info are
// rejected for CORS_ORIGINS; PUBLIC_URL may carry a path, which is dropped.
func ParseOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must be an absolute http(s) origin, got %q", raw)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// SSRFProtectionEnabled reports whether check and notification targets in
// private networks are blocked. Auto enables it for the Zitadel profile, which
// is the former zitadel mode; AuthZitadel is still honored for callers that
// build a Config by hand.
func (c Config) SSRFProtectionEnabled() bool {
	zitadel := c.AuthMode == AuthZitadel || (c.AuthMode == AuthOIDC && c.OIDC.Profile == ProfileZitadel)
	return c.SSRFProtection == "on" || (c.SSRFProtection == "auto" && zitadel)
}

// loadOIDC reads and validates the OIDC_* and ZITADEL_* variables for
// AUTH_MODE=oidc (ADR 0027). alias is set when AUTH_MODE=zitadel selected the
// mode, which fixes the profile to zitadel. Variables that belong to the other
// profile are errors instead of being ignored.
func loadOIDC(get func(key, def string) string, alias bool) (OIDCConfig, []error) {
	var errs []error
	var err error
	add := func(e error) {
		if e != nil {
			errs = append(errs, e)
		}
	}
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	var c OIDCConfig
	profile := OIDCProfile(get("OIDC_PROFILE", ""))
	switch {
	case alias:
		if profile != "" && profile != ProfileZitadel {
			fail("OIDC_PROFILE: AUTH_MODE=zitadel implies profile %q, got %q", ProfileZitadel, profile)
		}
		profile = ProfileZitadel
	case profile == "":
		profile = ProfileGeneric
	case profile != ProfileGeneric && profile != ProfileZitadel:
		fail("OIDC_PROFILE: must be %q or %q, got %q", ProfileGeneric, ProfileZitadel, profile)
		return c, errs
	}
	c.Profile = profile

	var autoErrs []error
	c.AutoProvision, c.AutoProvisionPlan, c.AutoProvisionMax, autoErrs = loadAutoProvision(get, profile)
	errs = append(errs, autoErrs...)

	c.InsecureHTTP, err = boolVar(get, "OIDC_INSECURE_HTTP", false)
	add(err)

	c.Issuer = get("OIDC_ISSUER", "")
	if c.Issuer == "" {
		fail("OIDC_ISSUER: required with AUTH_MODE=oidc")
	} else if e := checkIssuer(c.Issuer, c.InsecureHTTP); e != nil {
		fail("OIDC_ISSUER: %v", e)
	}

	c.Audience = get("OIDC_AUDIENCE", "")
	if c.Audience == "" {
		fail("OIDC_AUDIENCE: required with AUTH_MODE=oidc")
	}

	c.ClientIDs = splitList(get("OIDC_CLIENT_IDS", ""))
	if len(c.ClientIDs) == 0 {
		fail("OIDC_CLIENT_IDS: required with AUTH_MODE=oidc")
	} else {
		add(checkList("OIDC_CLIENT_IDS", c.ClientIDs))
	}

	c.RequireAZP, err = boolVar(get, "OIDC_REQUIRE_AZP", true)
	add(err)

	c.Algorithms = splitList(get("OIDC_ALGORITHMS", "RS256"))
	add(checkList("OIDC_ALGORITHMS", c.Algorithms))
	for _, alg := range c.Algorithms {
		if alg != "" && !isOIDCAlgorithm(alg) {
			fail("OIDC_ALGORITHMS: must be RS256, ES256 or EdDSA, got %q", alg)
		}
	}

	ttlRaw := get("OIDC_MAX_TOKEN_TTL", "1h")
	ttl, e := time.ParseDuration(ttlRaw)
	if e != nil || ttl <= 0 || ttl > 12*time.Hour {
		fail("OIDC_MAX_TOKEN_TTL: must be a duration above 0 and at most 12h, got %q", ttlRaw)
	}
	c.MaxTokenTTL = ttl

	if scopesRaw := get("OIDC_SCOPES", ""); scopesRaw == "" {
		c.Scopes = defaultScopes(profile, c.Audience)
	} else {
		c.Scopes = strings.FieldsFunc(scopesRaw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
		if !slices.Contains(c.Scopes, "openid") {
			fail("OIDC_SCOPES: must contain openid, got %q", scopesRaw)
		}
	}

	if profile == ProfileZitadel {
		if get("OIDC_ROLES_CLAIM", "") != "" {
			fail("OIDC_ROLES_CLAIM: not used with OIDC_PROFILE=zitadel, roles come from the project claim")
		}
		if get("OIDC_ROLE_MAP", "") != "" {
			fail("OIDC_ROLE_MAP: not used with OIDC_PROFILE=zitadel")
		}
		if get("OIDC_ROLES_SOURCE", "") != "" {
			fail("OIDC_ROLES_SOURCE: not used with OIDC_PROFILE=zitadel")
		}
		if get("OIDC_DEFAULT_ROLE", "") != "" {
			fail("OIDC_DEFAULT_ROLE: not used with OIDC_PROFILE=zitadel")
		}
		c.Tenancy = Tenancy(get("OIDC_TENANCY", string(TenancySingle)))
		if c.Tenancy != TenancySingle && c.Tenancy != TenancyMulti {
			fail("OIDC_TENANCY: must be %q or %q, got %q", TenancySingle, TenancyMulti, c.Tenancy)
		}
		c.PlatformOrgID = get("ZITADEL_PLATFORM_ORG_ID", "")
		if c.PlatformOrgID == "" {
			fail("ZITADEL_PLATFORM_ORG_ID: required with OIDC_PROFILE=zitadel")
		}
		return c, errs
	}

	if get("OIDC_TENANCY", "") != "" {
		fail("OIDC_TENANCY: only valid with OIDC_PROFILE=zitadel")
	}
	if get("ZITADEL_PLATFORM_ORG_ID", "") != "" {
		fail("ZITADEL_PLATFORM_ORG_ID: only valid with OIDC_PROFILE=zitadel")
	}
	c.Tenancy = TenancySingle
	c.RolesClaim = get("OIDC_ROLES_CLAIM", "")
	switch {
	case c.RolesClaim == "":
		fail("OIDC_ROLES_CLAIM: required with OIDC_PROFILE=generic")
	case !validClaimPath(c.RolesClaim):
		fail("OIDC_ROLES_CLAIM: must be a dot path with non-empty segments, got %q", c.RolesClaim)
	}
	c.RoleMap, err = parseRoleMap(get("OIDC_ROLE_MAP", ""))
	if err != nil {
		fail("OIDC_ROLE_MAP: %v", err)
	}
	c.RolesSource = get("OIDC_ROLES_SOURCE", RolesSourceAccessToken)
	if c.RolesSource != RolesSourceAccessToken && c.RolesSource != RolesSourceUserInfo {
		fail("OIDC_ROLES_SOURCE: must be %q or %q, got %q", RolesSourceAccessToken, RolesSourceUserInfo, c.RolesSource)
	}
	c.DefaultRole = get("OIDC_DEFAULT_ROLE", "")
	switch c.DefaultRole {
	case "", "tenant-admin", "tenant-editor", "tenant-viewer":
	case "platform-admin":
		fail("OIDC_DEFAULT_ROLE: platform-admin cannot be a default role")
	default:
		fail("OIDC_DEFAULT_ROLE: must be empty, tenant-admin, tenant-editor or tenant-viewer, got %q", c.DefaultRole)
	}
	return c, errs
}

// Defaults of the automatic provisioning (ADR 0029). DefaultAutoProvisionMax
// equals provision.DefaultMax.
const (
	DefaultAutoProvisionPlan = "default"
	DefaultAutoProvisionMax  = 100
)

// loadAutoProvision reads OIDC_AUTO_PROVISION, OIDC_AUTO_PROVISION_PLAN and
// OIDC_AUTO_PROVISION_MAX (ADR 0029). Enabling it needs the zitadel profile
// with multi tenancy; the plan and the maximum are only accepted with it on.
func loadAutoProvision(get func(key, def string) string, profile OIDCProfile) (bool, string, int, []error) {
	var errs []error
	enabled, err := boolVar(get, "OIDC_AUTO_PROVISION", false)
	if err != nil {
		errs = append(errs, err)
	}
	plan := get("OIDC_AUTO_PROVISION_PLAN", DefaultAutoProvisionPlan)
	maxTenants := DefaultAutoProvisionMax
	rawMax := get("OIDC_AUTO_PROVISION_MAX", "")
	if rawMax != "" {
		n, err := strconv.Atoi(rawMax)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("OIDC_AUTO_PROVISION_MAX: must be an integer of at least 1, got %q", rawMax))
		} else {
			maxTenants = n
		}
	}
	if !enabled {
		if get("OIDC_AUTO_PROVISION_PLAN", "") != "" || rawMax != "" {
			errs = append(errs, fmt.Errorf("OIDC_AUTO_PROVISION_PLAN and OIDC_AUTO_PROVISION_MAX: only valid with OIDC_AUTO_PROVISION=true"))
		}
		return false, plan, maxTenants, errs
	}
	if profile != ProfileZitadel || get("OIDC_TENANCY", "") != string(TenancyMulti) {
		errs = append(errs, fmt.Errorf("OIDC_AUTO_PROVISION: only valid with OIDC_PROFILE=zitadel and OIDC_TENANCY=multi"))
	}
	switch {
	case !tenant.ValidID(plan):
		errs = append(errs, fmt.Errorf("OIDC_AUTO_PROVISION_PLAN: must be 1 to 64 characters of a-z, 0-9 and '-', got %q", plan))
	case plan == tenant.SystemPlanID:
		errs = append(errs, fmt.Errorf("OIDC_AUTO_PROVISION_PLAN: the plan %q is reserved for the system tenant", plan))
	}
	return enabled, plan, maxTenants, errs
}

// boolVar reads a strict true or false, as the other boolean variables do.
func boolVar(get func(key, def string) string, key string, def bool) (bool, error) {
	switch v := get(key, strconv.FormatBool(def)); v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return def, fmt.Errorf("%s: must be true or false, got %q", key, v)
	}
}

// checkIssuer validates the form of OIDC_ISSUER: an absolute https URL, or http
// when insecure is set, without query or fragment. The value itself is not
// normalized, because the iss claim must match it exactly.
func checkIssuer(raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !u.IsAbs() {
		return fmt.Errorf("must be an absolute URL, got %q", raw)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && insecure:
	case u.Scheme == "http":
		return fmt.Errorf("http requires OIDC_INSECURE_HTTP=true, got %q", raw)
	default:
		return fmt.Errorf("scheme must be https, got %q", raw)
	}
	if strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("must not contain a query or fragment, got %q", raw)
	}
	return nil
}

// splitList splits a comma-separated list and trims each entry. An empty raw
// value yields nil.
func splitList(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// checkList rejects empty and duplicate entries of a list.
func checkList(key string, items []string) error {
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if item == "" {
			return fmt.Errorf("%s: must not contain empty entries", key)
		}
		if seen[item] {
			return fmt.Errorf("%s: duplicate entry %q", key, item)
		}
		seen[item] = true
	}
	return nil
}

// isOIDCAlgorithm reports whether alg is a signature algorithm OIDC_ALGORITHMS accepts.
func isOIDCAlgorithm(alg string) bool {
	switch alg {
	case "RS256", "ES256", "EdDSA":
		return true
	}
	return false
}

// isOIDCRole reports whether role is one of the four platform roles.
func isOIDCRole(role string) bool {
	switch role {
	case "platform-admin", "tenant-admin", "tenant-editor", "tenant-viewer":
		return true
	}
	return false
}

// defaultScopes returns the scopes requested when OIDC_SCOPES is not set.
// The zitadel profile adds the claims that carry the organization and the
// project roles; the project ID is the audience.
func defaultScopes(profile OIDCProfile, audience string) []string {
	scopes := []string{"openid", "profile", "offline_access"}
	if profile == ProfileZitadel {
		scopes = append(scopes,
			"urn:zitadel:iam:user:resourceowner",
			"urn:zitadel:iam:org:project:id:"+audience+":aud",
			"urn:zitadel:iam:org:projects:roles",
		)
	}
	return scopes
}

// validClaimPath reports whether p is a dot path such as realm_access.roles
// with non-empty segments.
func validClaimPath(p string) bool {
	for _, seg := range strings.Split(p, ".") {
		if seg == "" {
			return false
		}
	}
	return true
}

// parseRoleMap parses OIDC_ROLE_MAP in the form idpValue=role,idpValue2=role.
// An empty value yields nil.
func parseRoleMap(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	m := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		idp, role, ok := strings.Cut(strings.TrimSpace(entry), "=")
		idp, role = strings.TrimSpace(idp), strings.TrimSpace(role)
		if !ok || idp == "" {
			return nil, fmt.Errorf("entries must be idpValue=role, got %q", entry)
		}
		if _, dup := m[idp]; dup {
			return nil, fmt.Errorf("duplicate idpValue %q", idp)
		}
		if !isOIDCRole(role) {
			return nil, fmt.Errorf("role for %q must be platform-admin, tenant-admin, tenant-editor or tenant-viewer, got %q", idp, role)
		}
		m[idp] = role
	}
	return m, nil
}
