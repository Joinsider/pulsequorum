// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"fmt"
	"time"
)

// notificationSlack is the fixed allowance on top of the check timing in
// AS-01: notification dispatch, queueing and clock skew.
const notificationSlack = 30 * time.Second

// downDeadline is the AS-01 limit for the DOWN alert after the target fails:
// (retries+1) × interval + 30 s (pflichtenheft 13.2).
func downDeadline(interval time.Duration, retries int) (time.Duration, error) {
	if interval <= 0 {
		return 0, fmt.Errorf("interval must be positive, got %s", interval)
	}
	if retries < 0 {
		return 0, fmt.Errorf("retries must not be negative, got %d", retries)
	}
	return time.Duration(retries+1)*interval + notificationSlack, nil
}

// upDeadline bounds the wait for the first UP status of a new monitor. It is
// generous on purpose: three intervals plus the slack.
func upDeadline(interval time.Duration) time.Duration {
	return 3*interval + notificationSlack
}

// recoveryDeadline bounds the wait for the UP notification and the resolved
// incident after the target recovers. It uses the same limit as upDeadline.
func recoveryDeadline(interval time.Duration) time.Duration {
	return upDeadline(interval)
}
