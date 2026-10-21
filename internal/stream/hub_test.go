// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package stream

import (
	"sync"
	"testing"
)

func drain(s *Subscription) []string {
	var out []string
	for {
		select {
		case d, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, string(d))
		default:
			return out
		}
	}
}

// FA-TEN-05: events never cross tenants.
func TestHubTenantIsolation(t *testing.T) {
	h := NewHub(8)
	a, b := h.Subscribe("tenant-a"), h.Subscribe("tenant-b")
	defer a.Close()
	defer b.Close()

	h.Publish(Event{TenantID: "tenant-a", MonitorID: "m1", Data: []byte("a1")})
	h.Publish(Event{TenantID: "tenant-b", MonitorID: "m2", Data: []byte("b1")})
	h.Publish(Event{TenantID: "", MonitorID: "m3", Data: []byte("none")})

	if got := drain(a); len(got) != 1 || got[0] != "a1" {
		t.Errorf("tenant a got %v", got)
	}
	if got := drain(b); len(got) != 1 || got[0] != "b1" {
		t.Errorf("tenant b got %v", got)
	}
}

func TestHubEmptyTenantIsClosed(t *testing.T) {
	h := NewHub(8)
	s := h.Subscribe("")
	select {
	case <-s.Done():
	default:
		t.Fatal("subscription without tenant is open")
	}
	h.Publish(Event{TenantID: "", Data: []byte("x")})
	if h.Subscribers() != 0 {
		t.Errorf("subscribers = %d", h.Subscribers())
	}
	s.Close()
}

func TestHubMonitorFilter(t *testing.T) {
	h := NewHub(8)
	s := h.Subscribe("t")
	defer s.Close()
	s.SetMonitors([]string{"m1"})
	h.Publish(Event{TenantID: "t", MonitorID: "m1", Data: []byte("1")})
	h.Publish(Event{TenantID: "t", MonitorID: "m2", Data: []byte("2")})
	// A monitor ID of another tenant in the filter does not open that tenant.
	s.SetMonitors([]string{"m1", "foreign"})
	h.Publish(Event{TenantID: "other", MonitorID: "foreign", Data: []byte("3")})
	s.SetMonitors(nil)
	h.Publish(Event{TenantID: "t", MonitorID: "m2", Data: []byte("4")})
	if got := drain(s); len(got) != 2 || got[0] != "1" || got[1] != "4" {
		t.Errorf("got %v, want [1 4]", got)
	}
}

func TestHubDropsSlowSubscriber(t *testing.T) {
	h := NewHub(2)
	slow, fast := h.Subscribe("t"), h.Subscribe("t")
	defer fast.Close()
	for i := range 3 {
		h.Publish(Event{TenantID: "t", Data: []byte{byte('0' + i)}})
		drain(fast)
	}
	<-slow.Done()
	if !slow.Overflowed() {
		t.Error("slow subscriber not marked as overflowed")
	}
	if got := drain(slow); len(got) != 2 {
		t.Errorf("buffered events after drop = %v, want 2", got)
	}
	if fast.Overflowed() || h.Subscribers() != 1 {
		t.Errorf("fast overflowed=%v subscribers=%d", fast.Overflowed(), h.Subscribers())
	}
}

func TestHubCloseIsIdempotentAndRaceFree(t *testing.T) {
	h := NewHub(4)
	var wg sync.WaitGroup
	for range 20 {
		s := h.Subscribe("t")
		wg.Go(func() {
			for range 100 {
				h.Publish(Event{TenantID: "t", Data: []byte("x")})
			}
		})
		wg.Go(func() { drain(s); s.Close(); s.Close() })
	}
	wg.Wait()
	if n := h.Subscribers(); n != 0 {
		t.Errorf("subscribers = %d after close", n)
	}
}
