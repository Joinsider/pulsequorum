// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"net/url"
	"slices"
	"strings"
)

// Query parameters shown unmasked: they name recipients, not credentials.
var publicParams = []string{"chats", "channel", "channels", "to", "from", "fromaddress", "toaddresses", "title", "priority", "tags", "devices", "events", "rooms", "host", "port"}

// Mask replaces secrets in display, logs and API responses; in an update
// it means "keep the stored value".
const Mask = "***"

// MaskURL hides credentials of a channel URL for display and logs (FA-ALR-09):
// user info and path become *** and query values are masked unless they are
// known to be harmless. Unparsable input is masked completely.
func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return Mask
	}
	out := u.Scheme + "://"
	if u.User != nil {
		out += Mask + "@"
	}
	out += u.Host
	if strings.Trim(u.Path, "/") != "" || u.Opaque != "" {
		out += "/" + Mask
	}
	if len(u.RawQuery) > 0 {
		q := u.Query()
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			v := Mask
			if slices.Contains(publicParams, strings.ToLower(k)) {
				v = q.Get(k)
			}
			parts = append(parts, k+"="+v)
		}
		out += "?" + strings.Join(parts, "&")
	}
	return out
}

// secretParts returns the parts of a URL that may be credentials: the whole
// URL, user name, password, path segments and query values. Redact skips
// short parts to keep messages readable.
func secretParts(raw string) []string {
	parts := []string{raw}
	u, err := url.Parse(raw)
	if err != nil {
		return parts
	}
	if u.User != nil {
		parts = append(parts, u.User.Username())
		if p, ok := u.User.Password(); ok {
			parts = append(parts, p)
		}
	}
	parts = append(parts, strings.Split(u.Path, "/")...)
	for _, vs := range u.Query() {
		parts = append(parts, vs...)
	}
	parts = append(parts, u.Opaque, u.RawQuery)
	return parts
}

// Redact removes every secret (at least 4 characters long) and its URL-escaped
// form from msg, longest first.
func Redact(msg string, secrets ...string) string {
	var all []string
	for _, s := range secrets {
		if len(s) >= 4 {
			all = append(all, s, url.QueryEscape(s), url.PathEscape(s))
		}
	}
	slices.SortFunc(all, func(a, b string) int { return len(b) - len(a) })
	for _, s := range all {
		msg = strings.ReplaceAll(msg, s, Mask)
	}
	return msg
}
