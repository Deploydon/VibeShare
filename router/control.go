package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// EventBus is a tiny fan-out used to push "something changed" to SSE clients.
type EventBus struct {
	mu   sync.Mutex
	subs map[chan struct{}]bool
}

func newEventBus() *EventBus {
	return &EventBus{subs: map[chan struct{}]bool{}}
}

func (e *EventBus) Subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	e.mu.Lock()
	e.subs[ch] = true
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		delete(e.subs, ch)
		e.mu.Unlock()
	}
}

func (e *EventBus) Notify() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// ControlServer exposes the local management API consumed by the Swift app.
type ControlServer struct {
	store    *Store
	host     *HostManager
	guest    *GuestManager
	upstream *Upstream
	nostr    *NostrClient
	bus      *EventBus
	activity *ActivityLog
	subUsage *SubscriptionMonitor
}

func (c *ControlServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", c.getStatus)
	mux.HandleFunc("GET /api/grants", c.listGrants)
	mux.HandleFunc("POST /api/grants", c.createGrant)
	mux.HandleFunc("PATCH /api/grants/{id}", c.updateGrant)
	mux.HandleFunc("DELETE /api/grants/{id}", c.deleteGrant)
	mux.HandleFunc("GET /api/connections", c.listConnections)
	mux.HandleFunc("POST /api/connections", c.createConnection)
	mux.HandleFunc("DELETE /api/connections/{id}", c.deleteConnection)
	mux.HandleFunc("GET /api/config", c.getConfig)
	mux.HandleFunc("PUT /api/config", c.putConfig)
	mux.HandleFunc("POST /api/usage/{provider}/refresh", c.refreshUsage)
	mux.HandleFunc("GET /api/events", c.events)
	mux.HandleFunc("GET /api/activity", c.getActivity)
	return loopbackGuard(mux)
}

type relayStatus struct {
	URL       string `json:"url"`
	Connected bool   `json:"connected"`
}

func (c *ControlServer) getStatus(w http.ResponseWriter, r *http.Request) {
	cfg := c.store.Config()
	connected := map[string]bool{}
	for _, u := range c.nostr.ConnectedRelays() {
		connected[u] = true
	}
	relays := make([]relayStatus, 0, len(cfg.NostrRelays))
	for _, u := range cfg.NostrRelays {
		relays = append(relays, relayStatus{URL: u, Connected: connected[u]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"running":             true,
		"frontPort":           cfg.FrontPort,
		"controlPort":         cfg.ControlPort,
		"identityName":        cfg.IdentityName,
		"sharingEnabled":      cfg.EnableSharing,
		"autoStopSharing":     cfg.AutoStopSharing,
		"usageReservePercent": cfg.UsageReservePercent,
		"shareUsageLevel":     cfg.shareUsageLevel(),
		"upstream": map[string]any{
			"url":       cfg.UpstreamURL,
			"reachable": c.upstream.Reachable(),
		},
		"nostr":         map[string]any{"relays": relays},
		"localModels":   c.upstream.ListModelIDs(),
		"providerUsage": c.subUsage.MaybeRefresh(),
	})
}

type grantView struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	Code             string   `json:"code"`
	Providers        []string `json:"providers"`
	Models           []string `json:"models"`
	CreatedAt        int64    `json:"createdAt"`
	Revoked          bool     `json:"revoked"`
	Paused           bool     `json:"paused"`
	Online           bool     `json:"online"`
	Routing          bool     `json:"routing"`
	UsageLimited     bool     `json:"usageLimited"`     // fully auto-paused: reserve gate hid ALL models
	LimitedProviders []string `json:"limitedProviders"` // providers the reserve gate paused (partial or full)
	AdvertisedModels []string `json:"advertisedModels"`
	TotalReqs        int      `json:"totalReqs"`
	InputTokens      int64    `json:"inputTokens"`
	OutputTokens     int64    `json:"outputTokens"`
	TokenLimit       int64    `json:"tokenLimit"` // 0 = unlimited
}

func (c *ControlServer) listGrants(w http.ResponseWriter, r *http.Request) {
	out := []grantView{}
	for _, g := range c.store.Grants() {
		st := c.host.Status(g.ID)
		out = append(out, grantView{
			ID:               g.ID,
			Label:            g.Label,
			Code:             formatCode(g.Code),
			Providers:        nonNil(g.Providers),
			Models:           nonNil(g.Models),
			CreatedAt:        g.CreatedAt,
			Revoked:          g.Revoked,
			Paused:           g.Paused,
			Online:           st.Online,
			Routing:          st.Routing,
			UsageLimited:     st.UsageLimited,
			LimitedProviders: nonNil(st.LimitedProviders),
			AdvertisedModels: nonNil(st.Models),
			TotalReqs:        st.TotalReqs,
			InputTokens:      st.InputTokens,
			OutputTokens:     st.OutputTokens,
			TokenLimit:       g.TokenLimit,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *ControlServer) createGrant(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label      string   `json:"label"`
		Providers  []string `json:"providers"`
		Models     []string `json:"models"`
		TokenLimit int64    `json:"tokenLimit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	code, err := newGrantCode()
	if err != nil {
		http.Error(w, "code gen failed", http.StatusInternalServerError)
		return
	}
	label := in.Label
	if label == "" {
		label = "Friend"
	}
	limit := in.TokenLimit
	if limit < 0 {
		limit = 0
	}
	g := Grant{
		ID:         randomID(8),
		Label:      label,
		Code:       code,
		Providers:  in.Providers,
		Models:     in.Models,
		TokenLimit: limit,
		CreatedAt:  time.Now().Unix(),
	}
	if err := c.store.AddGrant(g); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	c.host.Reconcile()
	c.bus.Notify()
	writeJSON(w, http.StatusOK, grantView{
		ID:               g.ID,
		Label:            g.Label,
		Code:             formatCode(g.Code),
		Providers:        nonNil(g.Providers),
		Models:           nonNil(g.Models),
		CreatedAt:        g.CreatedAt,
		TokenLimit:       g.TokenLimit,
		AdvertisedModels: []string{},
	})
}

// updateGrant changes a grant's token allotment (top-up / adjust) and/or pauses
// or resumes it after the code was issued. Fields are optional — only those
// present are applied. Enforcement and presence read both live, so changes take
// effect on the next request without restarting the grant.
func (c *ControlServer) updateGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		TokenLimit *int64 `json:"tokenLimit"`
		Paused     *bool  `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	apply := func(err error) bool {
		if err == nil {
			return true
		}
		if errors.Is(err, errGrantNotFound) {
			http.Error(w, "grant not found", http.StatusNotFound)
		} else {
			http.Error(w, "save failed", http.StatusInternalServerError)
		}
		return false
	}
	if in.TokenLimit != nil && !apply(c.store.SetGrantLimit(id, *in.TokenLimit)) {
		return
	}
	if in.Paused != nil && !apply(c.store.SetGrantPaused(id, *in.Paused)) {
		return
	}
	if in.TokenLimit != nil || in.Paused != nil {
		// Push the new state to the guest right away instead of waiting for the
		// next 20s presence tick (presence carries paused + limit + usage, which
		// guests use to rank hosts when several share a model).
		c.host.RefreshGrant(id)
	}
	c.bus.Notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (c *ControlServer) deleteGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := c.store.RevokeGrant(id); err != nil {
		if errors.Is(err, errGrantNotFound) {
			http.Error(w, "grant not found", http.StatusNotFound)
		} else {
			http.Error(w, "save failed", http.StatusInternalServerError)
		}
		return
	}
	c.host.Reconcile()
	c.bus.Notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type connectionView struct {
	ID               string               `json:"id"`
	Label            string               `json:"label"`
	RedeemedAt       int64                `json:"redeemedAt"`
	Online           bool                 `json:"online"`
	Routing          bool                 `json:"routing"`
	Revoked          bool                 `json:"revoked"`
	Paused           bool                 `json:"paused"`                     // host paused sharing (still online)
	PausedReason     string               `json:"pausedReason,omitempty"`     // why, if the host said (e.g. usage limit)
	LimitedProviders []string             `json:"limitedProviders,omitempty"` // providers auto-paused by the host's reserve
	Usage            []providerUsageShare `json:"usage,omitempty"`            // host session windows, as far as they opted to share
	HostName         string               `json:"hostName"`
	Models           []string             `json:"models"`
	TotalReqs        int                  `json:"totalReqs"`
	InputTokens      int64                `json:"inputTokens"`
	OutputTokens     int64                `json:"outputTokens"`
	TokenLimit       int64                `json:"tokenLimit"` // host-advertised allotment (0 = unlimited)
	TokensUsed       int64                `json:"tokensUsed"` // host-authoritative usage
}

func (c *ControlServer) listConnections(w http.ResponseWriter, r *http.Request) {
	out := []connectionView{}
	for _, conn := range c.store.Connections() {
		st := c.guest.Status(conn.ID)
		revoked := conn.Revoked || st.Revoked
		models := st.Models
		if revoked {
			models = nil
		}
		out = append(out, connectionView{
			ID:               conn.ID,
			Label:            conn.Label,
			RedeemedAt:       conn.RedeemedAt,
			Online:           !revoked && st.Online,
			Routing:          !revoked && st.Routing,
			Revoked:          revoked,
			Paused:           !revoked && st.Paused,
			PausedReason:     st.PausedReason,
			LimitedProviders: nonNil(st.LimitedProviders),
			Usage:            st.Usage,
			HostName:         st.HostName,
			Models:           nonNil(models),
			TotalReqs:        st.TotalReqs,
			InputTokens:      st.InputTokens,
			OutputTokens:     st.OutputTokens,
			TokenLimit:       st.TokenLimit,
			TokensUsed:       st.TokensUsed,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *ControlServer) createConnection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if _, err := deriveGrantKeys(in.Code); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid code"})
		return
	}
	label := in.Label
	if label == "" {
		label = "Friend"
	}
	conn := Connection{
		ID:         randomID(8),
		Label:      label,
		Code:       normalizeCode(in.Code),
		RedeemedAt: time.Now().Unix(),
	}
	if err := c.store.AddConnection(conn); err != nil {
		if errors.Is(err, errConnectionExists) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "connection already exists"})
		} else {
			http.Error(w, "save failed", http.StatusInternalServerError)
		}
		return
	}
	c.guest.Reconcile()
	c.bus.Notify()
	writeJSON(w, http.StatusOK, connectionView{
		ID:         conn.ID,
		Label:      conn.Label,
		RedeemedAt: conn.RedeemedAt,
		Models:     []string{},
	})
}

func (c *ControlServer) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := c.store.RemoveConnection(id); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	c.guest.Reconcile()
	c.bus.Notify()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (c *ControlServer) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, c.store.Config())
}

// refreshUsage forces an immediate provider-usage fetch (the UI's refresh
// button), bypassing the 10-minute cache, and returns the fresh snapshot.
func (c *ControlServer) refreshUsage(w http.ResponseWriter, r *http.Request) {
	snap, ok := c.subUsage.Refresh(r.PathValue("provider"))
	if !ok {
		http.Error(w, "unknown provider", http.StatusBadRequest)
		return
	}
	c.bus.Notify() // nudge SSE clients to re-read the fresh usage
	writeJSON(w, http.StatusOK, snap)
}

func (c *ControlServer) putConfig(w http.ResponseWriter, r *http.Request) {
	cfg := c.store.Config()
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	// Keep the reserve in a sane band; the gate no-ops outside (0,100) anyway.
	if cfg.UsageReservePercent < 0 {
		cfg.UsageReservePercent = 0
	}
	if cfg.UsageReservePercent > 95 {
		cfg.UsageReservePercent = 95
	}
	// Collapse an unknown level to the default rather than persisting a value
	// nothing reads — otherwise a typo would silently disable the disclosure.
	cfg.ShareUsageLevel = cfg.shareUsageLevel()
	if err := c.store.SetConfig(cfg); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	// Re-evaluate every grant's shared models now (reserve / sharing toggles) so
	// the change lands immediately instead of on the next 20s presence tick.
	c.host.RefreshAll()
	c.bus.Notify()
	writeJSON(w, http.StatusOK, cfg)
}

// getActivity returns recent routed requests, newest first (?limit=N, default 50).
func (c *ControlServer) getActivity(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, c.activity.List(limit))
}

func (c *ControlServer) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := c.bus.Subscribe()
	defer unsub()

	fmt.Fprintf(w, "data: {\"type\":\"hello\"}\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprintf(w, "data: {\"type\":\"changed\"}\n\n")
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
