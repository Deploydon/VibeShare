package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type resetTransport func(*http.Request) (*http.Response, error)

func (f resetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBankedResetsParseUnavailableZeroAndPrivacy(t *testing.T) {
	for _, input := range []string{`null`, `{}`, `{"available_count":-1}`, `{"available_count":true}`, `{"available_count":1.5}`} {
		if got := parseBankedResets(json.RawMessage(input)); got != nil {
			t.Fatalf("%s: %+v", input, got)
		}
	}
	got := parseBankedResets(json.RawMessage(`{"available_count":2,"credits":[{"status":"available","expires_at":"2026-11-06T22:54:17.212500Z","id":"SECRET"},{"status":"redeemed","expires_at":"2026-10-01T00:00:00Z"},{"status":"available","expires_at":"2026-10-29T18:22:25.340739Z"}]}`))
	b, _ := json.Marshal(got)
	if got.Count != 2 || len(got.Expiries) != 2 || got.Expiries[0] != "2026-10-29T18:22:25Z" || strings.Contains(string(b), "SECRET") {
		t.Fatalf("%s", b)
	}
	zero := parseBankedResets(json.RawMessage(`{"available_count":0}`))
	if zero == nil || zero.Count != 0 || zero.Expiries == nil {
		t.Fatal("lost verified zero")
	}
	missing := parseBankedResets(json.RawMessage(`{"available_count":2,"credits":[{"status":"available","expires_at":"invalid"}]}`))
	if missing.Count != 2 || len(missing.Expiries) != 1 || missing.Expiries[0] != "" {
		t.Fatal("invented expiry")
	}
}

func TestCodexCreditListFailurePreservesUsageAndCount(t *testing.T) {
	for _, status := range []int{200, 401, 429, 503} {
		c := &http.Client{Transport: resetTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("ChatGPT-Account-Id") != "account" {
				t.Fatal("unexpected request")
			}
			body := `{"rate_limit":{"primary_window":{"used_percent":12,"reset_at":1792886598}},"rate_limit_reset_credits":{"available_count":2}}`
			code := 200
			if strings.HasSuffix(r.URL.Path, "rate-limit-reset-credits") {
				code = status
				body = `{"available_count":2,"credits":[{"status":"available","expires_at":"2026-10-29T18:22:25Z"}]}`
			}
			return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
		usage, _, err := fetchCodexUsage(c, rawCred{AccessToken: "secret", AccountID: "account"})
		if err != nil || len(usage.Windows) != 1 || usage.Windows[0].Utilization != 12 || usage.BankedResets.Count != 2 {
			t.Fatalf("status %d: %+v %v", status, usage, err)
		}
		if usage.Windows[0].ResetsAt != "2026-10-25T00:03:18Z" {
			t.Fatal(usage.Windows[0].ResetsAt)
		}
		if status != 200 && len(usage.BankedResets.Expiries) != 0 {
			t.Fatal("invented expiry after error")
		}
	}
}

func TestResetDisclosureOptInScopeAndImmediateRemoval(t *testing.T) {
	m := bothProviders()
	m.sources = nil
	m.snaps["codex"] = ProviderUsage{UpdatedAt: 123, Windows: []UsageWindow{{Label: "Session (5h)", Utilization: 12}}, BankedResets: &BankedResets{Count: 2, Expiries: []string{"2026-10-29T18:22:25Z"}}, Error: "SECRET"}
	h := hostAt(shareUsageOff, m)
	h.store.config.EnableSharing = true
	hg := &hostGrant{grant: Grant{ID: "codex", Providers: []string{"codex"}}, models: []string{"gpt-6-sol"}}
	for _, on := range []bool{false, true, false, true} {
		h.store.config.ShareCodexResetCredits = on
		pc := h.buildPresence(hg)
		got := pc.ProviderUsage["codex"]
		if (got.BankedResets != nil) != on || len(got.Windows) != 0 || got.Error != "" {
			t.Fatalf("toggle %v: %+v", on, pc)
		}
	}
	hg.grant.Providers = []string{"claude"}
	if got := h.buildPresence(hg).ProviderUsage["codex"]; got.BankedResets != nil {
		t.Fatal("ungranted account leaked")
	}
	hg.grant.Providers = []string{"codex"}
	hg.grant.Paused = true
	h.store.grants = []Grant{hg.grant}
	if got := h.buildPresence(hg); len(got.ProviderUsage) != 0 {
		t.Fatal("paused leak")
	}
	// Sanitization must deep copy rather than mutate the cached account snapshot.
	out := disclosedUsageForGrant(Grant{}, m.snaps, false, true)
	out["codex"].BankedResets.Expiries[0] = "changed"
	if m.snaps["codex"].BankedResets.Expiries[0] == "changed" {
		t.Fatal("mutated cache")
	}
}

func TestResetConfigDefaultPersistenceAndPartialUpdate(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config().ShareCodexResetCredits {
		t.Fatal("not default off")
	}
	cfg := s.Config()
	cfg.IdentityName = "Preserved"
	cfg.PreferBorrowedModels = true
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	h := &HostManager{store: s, grants: map[string]*hostGrant{}}
	control := &ControlServer{store: s, host: h, bus: newEventBus()}
	for _, on := range []bool{true, false, true} {
		body := `{"shareCodexResetCredits":false}`
		if on {
			body = `{"shareCodexResetCredits":true}`
		}
		w := httptest.NewRecorder()
		control.putConfig(w, httptest.NewRequest("PUT", "/api/config", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		reloaded, e := openStore(dir)
		if e != nil {
			t.Fatal(e)
		}
		if reloaded.Config().ShareCodexResetCredits != on || !reloaded.Config().PreferBorrowedModels || reloaded.Config().IdentityName != "Preserved" {
			t.Fatal("config lost")
		}
	}
	// A failed atomic config save must keep the previous opt-in in memory.
	s.dir = filepath.Join(dir, "nonexistent", "nope")
	cfg = s.Config()
	cfg.ShareCodexResetCredits = false
	if s.SetConfig(cfg) == nil || !s.Config().ShareCodexResetCredits {
		t.Fatal("failed save changed live privacy")
	}
}

func TestDesktopCodexCredentialFallbackReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "auth.json")
	data := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"secret","account_id":"desktop"}}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cr, err := desktopCodexCred()
	if err != nil || cr.token() != "secret" || cr.account() != "desktop" {
		t.Fatal("desktop read failed")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("auth mutated")
	}
	os.WriteFile(path, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"secret"}`), 0600)
	if _, err := desktopCodexCred(); err == nil {
		t.Fatal("API key treated as OAuth")
	}
}

func TestResetRefreshAccountSwitchFailureRetryAndZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-test.json")
	write := func(account string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(`{"account_id":"`+account+`","access_token":"secret"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := newSubscriptionMonitor(dir)
	m.pstate["codex"] = &provState{}
	fail := false
	count := 2
	src := usageSource{key: "codex", credFile: credMatch("codex-"), fetch: func(*http.Client, rawCred) (ProviderUsage, int, error) {
		if fail {
			return ProviderUsage{}, 503, io.ErrUnexpectedEOF
		}
		return ProviderUsage{Windows: []UsageWindow{{Label: "Weekly", Utilization: 10}}, BankedResets: &BankedResets{Count: count, Expiries: []string{}}}, 200, nil
	}}
	write("first")
	m.refresh(src)
	if m.snaps["codex"].BankedResets.Count != 2 {
		t.Fatal("initial read")
	}
	write("second")
	fail = true
	m.refresh(src)
	if m.snaps["codex"].UpdatedAt != 0 || m.snaps["codex"].BankedResets != nil || len(m.snaps["codex"].Windows) != 0 {
		t.Fatal("previous account survived switch")
	}
	fail = false
	count = 0
	m.refresh(src)
	if m.snaps["codex"].BankedResets == nil || m.snaps["codex"].BankedResets.Count != 0 {
		t.Fatal("retry lost verified zero")
	}
	fail = true
	m.refresh(src)
	if m.snaps["codex"].BankedResets != nil || len(m.snaps["codex"].Windows) != 1 {
		t.Fatal("failed refresh leaked credits or lost usage")
	}
}
