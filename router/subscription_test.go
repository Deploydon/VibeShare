package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestClaudeUsagePrefersNamedStructuredLimits(t *testing.T) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"five_hour":{"utilization":99},
		"iguana_necktie":{"utilization":12},
		"nimbus_quill":{"utilization":13},
		"limits":[
			{"kind":"session","percent":1,"resets_at":null},
			{"kind":"weekly_all","percent":22},
			{"kind":"weekly_scoped","percent":33,"scope":{"model":{"display_name":"Fable"}}},
			{"kind":"weekly_scoped","percent":44,"scope":{"model":{"display_name":"Sonnet"}}},
			{"kind":"weekly_scoped","percent":0,"scope":null},
			{"kind":"future","percent":50},
			{"kind":"session","percent":null},
			"malformed"
		]
	}`), &raw); err != nil {
		t.Fatal(err)
	}
	got := parseClaudeWindows(raw)
	want := []UsageWindow{
		{Label: "Session (5h)", Utilization: 1},
		{Label: "Weekly", Utilization: 22},
		{Label: "Weekly · Fable", Utilization: 33},
		{Label: "Weekly · Sonnet", Utilization: 44},
		{Label: "Weekly · Unidentified model", Utilization: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestClaudeLegacyUsageSkipsInternalBuckets(t *testing.T) {
	for _, limits := range []string{"null", "[]", `[{"kind":"future","percent":50}]`, `"invalid"`} {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(`{
			"five_hour":{"utilization":0},
			"seven_day_sonnet":{"utilization":1},
			"iguana_necktie":{"utilization":12},
			"nimbus_quill":{"utilization":13},
			"seven_day_omelette":{"utilization":15},
			"extra_usage":{"utilization":14},
			"limits":`+limits+`
		}`), &raw); err != nil {
			t.Fatal(err)
		}
		got := parseClaudeWindows(raw)
		if len(got) != 2 || got[0].Label != "Session (5h)" || got[1].Label != "Weekly · Sonnet" {
			t.Fatalf("limits %s: %+v", limits, got)
		}
	}
}

func TestClaudePartialLimitsPreserveLegacySessionReserve(t *testing.T) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"five_hour":{"utilization":95,"resets_at":"2026-10-01T10:00:00Z"},
		"seven_day":{"utilization":20},
		"seven_day_fable":{"utilization":99},
		"limits":[
			{"kind":"session","percent":"malformed"},
			{"kind":"weekly_scoped","percent":12,"scope":{"model":{"display_name":"Fable"}}}
		]
	}`), &raw); err != nil {
		t.Fatal(err)
	}
	m := newSubscriptionMonitor("/nonexistent")
	m.snaps["claude"] = ProviderUsage{UpdatedAt: 1, Windows: parseClaudeWindows(raw)}
	if util, known := m.SessionUtilization("claude"); !known || util != 95 {
		t.Fatalf("lost reserve reading: %v, %v", util, known)
	}
	if got := m.SessionResetsAt("claude"); got != "2026-10-01T10:00:00Z" {
		t.Fatalf("lost reset time: %q", got)
	}
	shared := sharedUsageForGrant(Grant{Providers: []string{"claude"}}, m.snaps)
	if len(shared["claude"].Windows) != 3 || shared["claude"].Windows[0].Label != "Weekly · Fable" || shared["claude"].Windows[0].Utilization != 12 {
		t.Fatalf("incorrect shared windows: %+v", shared)
	}
}

func TestSharedClaudeUsageFromOldHostDoesNotGuessModelNames(t *testing.T) {
	windows := []UsageWindow{
		{Label: "Session (5h)", Utilization: 1},
		{Label: "Weekly", Utilization: 31},
		{Label: "Iguana necktie", Utilization: 2, ResetsAt: "2026-11-05T07:59:00Z"},
		{Label: "Nimbus quill", Utilization: 3},
		{Label: "Weekly · Fable", Utilization: 4},
	}
	source := map[string]ProviderUsage{
		"claude": {UpdatedAt: 123, Windows: windows},
		"codex":  {UpdatedAt: 456, Windows: []UsageWindow{{Label: "Code review · Weekly", Utilization: 5}}},
	}
	got := normalizeSharedUsage(source)
	for i, label := range []string{"Session (5h)", "Weekly", "Unidentified Claude limit 1", "Unidentified Claude limit 2", "Weekly · Fable"} {
		want := windows[i]
		want.Label = label
		if got["claude"].Windows[i] != want {
			t.Fatalf("window %d = %+v, want %+v", i, got["claude"].Windows[i], want)
		}
	}
	if source["claude"].Windows[2].Label != "Iguana necktie" || got["claude"].UpdatedAt != 123 || !reflect.DeepEqual(got["codex"], source["codex"]) {
		t.Fatal("normalization changed source data, freshness, or another provider")
	}
	if !reflect.DeepEqual(normalizeSharedUsage(got), got) {
		t.Fatal("normalization is not idempotent")
	}
}

func TestClaudeUsageIncludesEveryAvailableWindow(t *testing.T) {
	var raw map[string]json.RawMessage
	json.Unmarshal([]byte(`{
		"five_hour":{"utilization":11,"resets_at":"2026-09-25T10:00:00Z"},
		"seven_day":{"utilization":22,"resets_at":"2026-09-30T10:00:00Z"},
		"seven_day_fable":{"utilization":33,"resets_at":"2026-09-30T10:00:00Z"},
		"seven_day_opus":{"utilization":44,"resets_at":"2026-09-30T10:00:00Z"},
		"extra_usage":{"is_enabled":true,"monthly_limit":100},
		"unknown":null
	}`), &raw)
	windows := parseClaudeWindows(raw)
	if len(windows) != 4 {
		t.Fatalf("got %d windows: %+v", len(windows), windows)
	}
	for i, want := range []string{"Session (5h)", "Weekly", "Weekly · Fable", "Weekly · Opus"} {
		if windows[i].Label != want {
			t.Errorf("window %d = %q, want %q", i, windows[i].Label, want)
		}
	}
}

func TestSharedUsageOnlyIncludesGrantedProviders(t *testing.T) {
	snaps := map[string]ProviderUsage{
		"claude": {UpdatedAt: 1, Error: "private provider response", Windows: []UsageWindow{{Label: "Weekly", Utilization: 50}}},
		"codex":  {UpdatedAt: 1, Windows: []UsageWindow{{Label: "Weekly", Utilization: 75}}},
	}
	for _, tc := range []struct {
		grant Grant
		want  int
		key   string
	}{
		{Grant{}, 2, ""},
		{Grant{Providers: []string{"claude"}}, 1, "claude"},
		{Grant{Models: []string{"claude-fable-5"}}, 1, "claude"},
	} {
		got := sharedUsageForGrant(tc.grant, snaps)
		if len(got) != tc.want {
			t.Errorf("grant %+v got %+v", tc.grant, got)
		}
		if tc.key != "" {
			if _, ok := got[tc.key]; !ok {
				t.Errorf("missing %s", tc.key)
			}
		}
		if got["claude"].Error != "" {
			t.Error("provider error leaked to a shared grant")
		}
	}
}

func TestCodexUsagePrefersPerLimitWindows(t *testing.T) {
	var raw map[string]json.RawMessage
	json.Unmarshal([]byte(`{
		"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":20}},
		"rate_limits_by_limit_id":{
			"codex":{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":20}}},
			"code_review":{"rate_limit":{"primary_window":{"used_percent":30}}}
		}
	}`), &raw)
	windows := parseCodexWindows(raw)
	if len(windows) != 3 {
		t.Fatalf("got %d windows: %+v", len(windows), windows)
	}
}
