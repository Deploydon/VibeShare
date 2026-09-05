package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProviderUsage is one provider's subscription window state, surfaced to the UI.
// UpdatedAt is 0 until the first successful fetch; Error carries the last
// failure reason (without leaking the token) so the UI can show it inline.
type ProviderUsage struct {
	UpdatedAt int64         `json:"updatedAt"`
	Error     string        `json:"error,omitempty"`
	Windows   []UsageWindow `json:"windows,omitempty"`
}

// UsageWindow is a single rate-limit window: percent used + reset timestamp.
type UsageWindow struct {
	Label       string  `json:"label"`
	Utilization float64 `json:"utilization"` // percent used, 0..100
	ResetsAt    string  `json:"resetsAt"`    // RFC3339, "" if unknown
}

// SubscriptionMonitor polls each connected provider's own usage endpoint for the
// host's subscription limits, reusing the OAuth tokens cli-proxy-api already
// maintains on disk. Those endpoints are aggressively rate-limited, so each
// provider is cached and refreshed at most every refreshTTL, with a hard backoff
// on a 429.
type SubscriptionMonitor struct {
	authDir string
	client  *http.Client
	sources []usageSource

	mu     sync.Mutex
	snaps  map[string]ProviderUsage
	pstate map[string]*provState
}

type provState struct {
	refreshing   bool
	backoffUntil int64
}

// usageSource describes how to fetch one provider's usage.
type usageSource struct {
	key      string                 // matches the UI provider key ("claude", "codex")
	credFile func(name string) bool // matches that provider's cred files in authDir
	fetch    func(c *http.Client, cr rawCred) (ProviderUsage, int, error)
}

const (
	refreshTTL = 10 * time.Minute
	backoff429 = 30 * time.Minute
	// manualRefreshMinInterval throttles the UI's "refresh now" button so a
	// spammed click can't hammer the provider's rate-limited usage endpoint. It
	// still bypasses the 10-minute staleness TTL (the button's whole point), just
	// not a fetch from the last few seconds or an active 429/soft backoff.
	manualRefreshMinInterval = 20 * time.Second
)

var (
	errNoCreds      = errors.New("not connected")
	errTokenExpired = errors.New("token expired — make a request to refresh it")
)

func newSubscriptionMonitor(authDir string) *SubscriptionMonitor {
	return &SubscriptionMonitor{
		authDir: expandHome(authDir),
		client:  &http.Client{Timeout: 10 * time.Second},
		sources: []usageSource{
			{key: "claude", credFile: credMatch("claude-"), fetch: fetchClaudeUsage},
			// Codex stays inert until codex creds exist.
			{key: "codex", credFile: credMatchAny("codex-", "chatgpt-"), fetch: fetchCodexUsage},
		},
		snaps:  map[string]ProviderUsage{},
		pstate: map[string]*provState{},
	}
}

// MaybeRefresh kicks off an async refresh for any provider whose cache is stale
// and not backed off, then returns the current per-provider snapshots. Called
// from the status handler (polled by the UI), so usage refreshes only while the
// app is open and never faster than refreshTTL per provider.
func (m *SubscriptionMonitor) MaybeRefresh() map[string]ProviderUsage {
	now := time.Now().Unix()
	m.mu.Lock()
	for _, src := range m.sources {
		st := m.pstate[src.key]
		if st == nil {
			st = &provState{}
			m.pstate[src.key] = st
		}
		stale := now-m.snaps[src.key].UpdatedAt >= int64(refreshTTL.Seconds())
		if stale && !st.refreshing && now >= st.backoffUntil {
			st.refreshing = true
			go m.refresh(src)
		}
	}
	out := make(map[string]ProviderUsage, len(m.snaps))
	for k, v := range m.snaps {
		out[k] = v
	}
	m.mu.Unlock()
	return out
}

// SessionUtilization returns the provider's most-recent 5-hour session window
// usage (0..100) and whether it is known. It reads the cached snapshot only —
// the UI's status poll already drives refreshes — so the host's sharing gate
// never adds network calls. known is false when the provider has never been
// polled successfully or exposes no session window, so callers can fail open.
func (m *SubscriptionMonitor) SessionUtilization(provider string) (float64, bool) {
	m.mu.Lock()
	snap, ok := m.snaps[provider]
	m.mu.Unlock()
	if !ok || snap.UpdatedAt == 0 {
		return 0, false
	}
	for _, w := range snap.Windows {
		if strings.HasPrefix(strings.ToLower(w.Label), "session") {
			return w.Utilization, true
		}
	}
	return 0, false
}

// SessionResetsAt returns the RFC3339 reset timestamp of the provider's 5-hour
// session window, or "" when unknown. Cache-only, like SessionUtilization: it
// adds no network calls to the presence path.
func (m *SubscriptionMonitor) SessionResetsAt(provider string) string {
	m.mu.Lock()
	snap, ok := m.snaps[provider]
	m.mu.Unlock()
	if !ok || snap.UpdatedAt == 0 {
		return ""
	}
	for _, w := range snap.Windows {
		if strings.HasPrefix(strings.ToLower(w.Label), "session") {
			return w.ResetsAt
		}
	}
	return ""
}

// Refresh forces an immediate, synchronous usage fetch for one provider,
// bypassing the staleness TTL while still respecting active refreshes and
// provider backoff. Returns the resulting snapshot and whether the provider key
// is known.
func (m *SubscriptionMonitor) Refresh(provider string) (ProviderUsage, bool) {
	var src usageSource
	found := false
	for _, s := range m.sources {
		if s.key == provider {
			src, found = s, true
			break
		}
	}
	if !found {
		return ProviderUsage{}, false
	}
	now := time.Now().Unix()
	m.mu.Lock()
	st := m.pstate[src.key]
	if st == nil {
		st = &provState{}
		m.pstate[src.key] = st
	}
	snap := m.snaps[src.key]
	// Skip the fetch if one is already in flight, if we're inside a 429/soft
	// backoff (protect the host's real token), or if a successful fetch landed
	// within the last interval — return the current snapshot in those cases.
	if st.refreshing ||
		now < st.backoffUntil ||
		(snap.UpdatedAt != 0 && now-snap.UpdatedAt < int64(manualRefreshMinInterval.Seconds())) {
		m.mu.Unlock()
		return snap, true
	}
	st.refreshing = true
	m.mu.Unlock()

	m.refresh(src) // synchronous; its defer clears refreshing and stores the result

	m.mu.Lock()
	snap = m.snaps[src.key]
	m.mu.Unlock()
	return snap, true
}

func (m *SubscriptionMonitor) refresh(src usageSource) {
	wait := time.Minute // retry delay after a soft failure (no creds, token issue)
	var resultErr error
	var usage ProviderUsage

	// One exit point: record success or failure, set the next-retry backoff, and
	// clear the refreshing flag. A failure keeps any previously fetched windows.
	defer func() {
		m.mu.Lock()
		st := m.pstate[src.key]
		st.refreshing = false
		if resultErr != nil {
			st.backoffUntil = time.Now().Add(wait).Unix()
			snap := m.snaps[src.key]
			snap.Error = resultErr.Error()
			m.snaps[src.key] = snap
		} else {
			usage.UpdatedAt = time.Now().Unix()
			m.snaps[src.key] = usage
		}
		m.mu.Unlock()
	}()

	cred, err := m.newestCred(src.credFile)
	if err != nil {
		resultErr = err
		return
	}
	if cred.token() == "" || cred.Disabled {
		resultErr = errNoCreds
		return
	}
	if t, e := time.Parse(time.RFC3339, cred.Expired); e == nil && time.Now().After(t) {
		resultErr = errTokenExpired
		return
	}
	u, status, err := src.fetch(m.client, cred)
	if err != nil {
		if status == http.StatusTooManyRequests {
			wait = backoff429
		}
		resultErr = err
		return
	}
	usage = u
}

// newestCred reads the freshest matching credential file in the auth dir.
func (m *SubscriptionMonitor) newestCred(match func(string) bool) (rawCred, error) {
	entries, err := os.ReadDir(m.authDir)
	if err != nil {
		return rawCred{}, errNoCreds
	}
	var newest os.DirEntry
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !match(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == nil || info.ModTime().After(newestMod) {
			newest = e
			newestMod = info.ModTime()
		}
	}
	if newest == nil {
		return rawCred{}, errNoCreds
	}
	data, err := os.ReadFile(filepath.Join(m.authDir, newest.Name()))
	if err != nil {
		return rawCred{}, errNoCreds
	}
	var c rawCred
	if json.Unmarshal(data, &c) != nil {
		return rawCred{}, errNoCreds
	}
	return c, nil
}

func credMatch(prefix string) func(string) bool {
	return func(n string) bool { return strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".json") }
}

func credMatchAny(prefixes ...string) func(string) bool {
	return func(n string) bool {
		if !strings.HasSuffix(n, ".json") {
			return false
		}
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				return true
			}
		}
		return false
	}
}

// rawCred is a tolerant superset of cli-proxy-api credential files (fields may
// be top-level or nested under "tokens", depending on provider).
type rawCred struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	Expired     string `json:"expired"`
	Disabled    bool   `json:"disabled"`
	Tokens      *struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

func (r rawCred) token() string {
	if r.AccessToken != "" {
		return r.AccessToken
	}
	if r.Tokens != nil {
		return r.Tokens.AccessToken
	}
	return ""
}

func (r rawCred) account() string {
	if r.AccountID != "" {
		return r.AccountID
	}
	if r.Tokens != nil {
		return r.Tokens.AccountID
	}
	return ""
}

// fetchClaudeUsage queries Anthropic's OAuth usage endpoint (verified working).
func fetchClaudeUsage(c *http.Client, cr rawCred) (ProviderUsage, int, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return ProviderUsage{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cr.token())
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", "claude-code/2.0.0")

	resp, err := c.Do(req)
	if err != nil {
		return ProviderUsage{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ProviderUsage{}, resp.StatusCode, errors.New("usage endpoint returned " + resp.Status)
	}
	var raw struct {
		FiveHour       *claudeWindow `json:"five_hour"`
		SevenDay       *claudeWindow `json:"seven_day"`
		SevenDayOpus   *claudeWindow `json:"seven_day_opus"`
		SevenDaySonnet *claudeWindow `json:"seven_day_sonnet"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return ProviderUsage{}, resp.StatusCode, err
	}
	var out ProviderUsage
	add := func(label string, w *claudeWindow) {
		if w != nil {
			out.Windows = append(out.Windows, UsageWindow{Label: label, Utilization: w.Utilization, ResetsAt: w.ResetsAt})
		}
	}
	add("Session (5h)", raw.FiveHour)
	add("Weekly", raw.SevenDay)
	add("Weekly · Opus", raw.SevenDayOpus)
	add("Weekly · Sonnet", raw.SevenDaySonnet)
	return out, resp.StatusCode, nil
}

type claudeWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// fetchCodexUsage queries ChatGPT's Codex usage endpoint. Originally written from
// public docs (CodexBar, openai/codex); since confirmed against a live Codex
// account, whose session window drives the reserve gate correctly. The response
// shape is still parsed defensively, since it is undocumented and may change.
func fetchCodexUsage(c *http.Client, cr rawCred) (ProviderUsage, int, error) {
	req, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return ProviderUsage{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cr.token())
	if acct := cr.account(); acct != "" {
		req.Header.Set("ChatGPT-Account-Id", acct)
	}
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0")
	req.Header.Set("Accept", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return ProviderUsage{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return ProviderUsage{}, resp.StatusCode, errors.New("usage endpoint returned " + resp.Status + ": " + strings.TrimSpace(string(body)))
	}
	var raw struct {
		RateLimit *struct {
			Primary   *codexWindow `json:"primary_window"`
			Secondary *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
		Primary         *codexWindow `json:"primary"`
		Secondary       *codexWindow `json:"secondary"`
		PrimaryWindow   *codexWindow `json:"primary_window"`
		SecondaryWindow *codexWindow `json:"secondary_window"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return ProviderUsage{}, resp.StatusCode, err
	}
	pick := func(opts ...*codexWindow) *codexWindow {
		for _, o := range opts {
			if o != nil {
				return o
			}
		}
		return nil
	}
	var primary, secondary *codexWindow
	if raw.RateLimit != nil {
		primary, secondary = raw.RateLimit.Primary, raw.RateLimit.Secondary
	}
	primary = pick(primary, raw.Primary, raw.PrimaryWindow)
	secondary = pick(secondary, raw.Secondary, raw.SecondaryWindow)

	var out ProviderUsage
	if w := primary.toUsage("Session (5h)"); w != nil {
		out.Windows = append(out.Windows, *w)
	}
	if w := secondary.toUsage("Weekly"); w != nil {
		out.Windows = append(out.Windows, *w)
	}
	if len(out.Windows) == 0 {
		return ProviderUsage{}, resp.StatusCode, errors.New("usage response had no recognizable windows")
	}
	return out, resp.StatusCode, nil
}

type codexWindow struct {
	UsedPercent     *float64 `json:"used_percent"`
	Utilization     *float64 `json:"utilization"`
	ResetsInSeconds *float64 `json:"resets_in_seconds"`
	ResetAfterSecs  *float64 `json:"reset_after_seconds"`
	ResetsAt        string   `json:"resets_at"`
}

func (w *codexWindow) toUsage(label string) *UsageWindow {
	if w == nil {
		return nil
	}
	util := 0.0
	switch {
	case w.UsedPercent != nil:
		util = *w.UsedPercent
	case w.Utilization != nil:
		util = *w.Utilization
	}
	reset := w.ResetsAt
	if reset == "" {
		secs := -1.0
		switch {
		case w.ResetsInSeconds != nil:
			secs = *w.ResetsInSeconds
		case w.ResetAfterSecs != nil:
			secs = *w.ResetAfterSecs
		}
		if secs >= 0 {
			reset = time.Now().Add(time.Duration(secs) * time.Second).Format(time.RFC3339)
		}
	}
	return &UsageWindow{Label: label, Utilization: util, ResetsAt: reset}
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
