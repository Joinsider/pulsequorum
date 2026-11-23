// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package membership

import (
	"testing"
	"time"
)

// TestManagerPromoteInterval checks that PromotePoll replaces promotePoll and
// that zero means promotePoll.
func TestManagerPromoteInterval(t *testing.T) {
	tests := []struct {
		name string
		poll time.Duration
		want time.Duration
	}{
		{name: "zero means promotePoll (2 s)", poll: 0, want: 2 * time.Second},
		{name: "non-zero is used as is", poll: 50 * time.Millisecond, want: 50 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{PromotePoll: tt.poll}
			if got := m.promoteInterval(); got != tt.want {
				t.Errorf("promoteInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}
