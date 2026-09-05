package main

import "testing"

func monitorWith(provider, label, resetsAt string, util float64) *SubscriptionMonitor {
	m := newSubscriptionMonitor("/nonexistent")
	m.snaps[provider] = ProviderUsage{
		UpdatedAt: 1,
		Windows:   []UsageWindow{{Label: label, Utilization: util, ResetsAt: resetsAt}},
	}
	return m
}

func TestSessionResetsAt(t *testing.T) {
	m := monitorWith("codex", "Session (5h)", "2026-09-05T21:15:00Z", 92)
	if got := m.SessionResetsAt("codex"); got != "2026-09-05T21:15:00Z" {
		t.Fatalf("session reset = %q", got)
	}
	// A provider never polled must not invent a time.
	if got := m.SessionResetsAt("claude"); got != "" {
		t.Fatalf("unpolled provider = %q, want empty", got)
	}
	// Weekly-only snapshots have no session window to report.
	weekly := newSubscriptionMonitor("/nonexistent")
	weekly.snaps["codex"] = ProviderUsage{UpdatedAt: 1, Windows: []UsageWindow{{Label: "Weekly", ResetsAt: "x"}}}
	if got := weekly.SessionResetsAt("codex"); got != "" {
		t.Fatalf("weekly-only = %q, want empty", got)
	}
}

func hostAt(level string, m *SubscriptionMonitor) *HostManager {
	return &HostManager{store: &Store{config: Config{ShareUsageLevel: level}}, subUsage: m}
}

// find returns the row for a provider display name, or nil.
func find(rows []providerUsageShare, name string) *providerUsageShare {
	for i := range rows {
		if rows[i].Provider == name {
			return &rows[i]
		}
	}
	return nil
}

// bothProviders is a monitor that knows a session window for claude and codex.
func bothProviders() *SubscriptionMonitor {
	m := monitorWith("codex", "Session (5h)", "2026-09-05T21:15:00Z", 92)
	m.snaps["claude"] = ProviderUsage{
		UpdatedAt: 1,
		Windows:   []UsageWindow{{Label: "Session (5h)", Utilization: 41, ResetsAt: "2026-09-05T23:00:00Z"}},
	}
	return m
}

func TestUsageShareOff(t *testing.T) {
	m := bothProviders()
	// Even with a provider visibly paused, "off" discloses nothing at all.
	if got := hostAt(shareUsageOff, m).usageShare([]string{"Codex"}, []string{"claude-opus-5"}); got != nil {
		t.Fatalf("off leaked: %#v", got)
	}
	// An empty/unknown level must behave as the "resets" default, not as off.
	if got := hostAt("", m).usageShare([]string{"Codex"}, nil); len(got) != 1 {
		t.Fatalf("empty level = %#v, want resets behaviour", got)
	}
	if got := hostAt("bogus", m).usageShare([]string{"Codex"}, nil); len(got) != 1 {
		t.Fatalf("unknown level = %#v, want resets behaviour", got)
	}
}

func TestUsageShareResets(t *testing.T) {
	h := hostAt(shareUsageResets, bothProviders())

	got := h.usageShare([]string{"Codex"}, []string{"claude-opus-5"})
	row := find(got, "Codex")
	if row == nil || row.ResetsAt != "2026-09-05T21:15:00Z" || !row.Limited {
		t.Fatalf("codex row = %#v", got)
	}
	// The whole point of this level: a time, never a number.
	if row.Utilization != nil {
		t.Fatalf("resets level leaked utilization: %v", *row.Utilization)
	}
	// Claude is shared and unlimited, so at this level it says nothing — the
	// disclosure is strictly a timestamp on a pause the guest already sees.
	if find(got, "Claude") != nil {
		t.Fatalf("resets level disclosed an unlimited provider: %#v", got)
	}
	// Nothing limited at all: nothing to time.
	if got := h.usageShare(nil, []string{"claude-opus-5", "gpt-6-astra"}); got != nil {
		t.Fatalf("nothing limited: %#v", got)
	}
	// A limited provider with no known window is omitted, not sent empty.
	unknown := hostAt(shareUsageResets, newSubscriptionMonitor("/nonexistent"))
	if got := unknown.usageShare([]string{"Codex"}, nil); got != nil {
		t.Fatalf("unknown window: %#v", got)
	}
}

func TestUsageShareWindows(t *testing.T) {
	h := hostAt(shareUsageWindows, bothProviders())

	// The advance warning this level exists for: Claude is fine, still shared,
	// and the guest can see how much room is left before it stops.
	got := h.usageShare(nil, []string{"claude-opus-5"})
	row := find(got, "Claude")
	if row == nil || row.Utilization == nil || *row.Utilization != 41 || row.Limited {
		t.Fatalf("claude row = %#v", got)
	}
	// A grant that shares no Codex models never learns Codex's numbers.
	if find(got, "Codex") != nil {
		t.Fatalf("leaked an unshared provider: %#v", got)
	}
	// Once paused, the provider is disclosed even though the gate pulled its
	// models out of the advertised list.
	got = h.usageShare([]string{"Codex"}, []string{"claude-opus-5"})
	if row := find(got, "Codex"); row == nil || !row.Limited || row.Utilization == nil {
		t.Fatalf("limited codex row = %#v", got)
	}
	// Order is the stable human one, so the UI doesn't reshuffle between polls.
	if len(got) != 2 || got[0].Provider != "Claude" || got[1].Provider != "Codex" {
		t.Fatalf("order = %#v", got)
	}
}

func TestUsageShareNilMonitor(t *testing.T) {
	// Usage never wired up: every level must no-op rather than panic.
	for _, level := range []string{shareUsageOff, shareUsageResets, shareUsageWindows} {
		if got := hostAt(level, nil).usageShare([]string{"Codex"}, []string{"gpt-6-astra"}); got != nil {
			t.Fatalf("%s with nil monitor: %#v", level, got)
		}
	}
}
