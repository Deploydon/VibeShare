package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pion/webrtc/v3"
)

// authWindow bounds how stale a data-channel auth/presence timestamp may be.
// Kept small to limit replay even though the channel is already DTLS-encrypted.
const authWindow = 90 * time.Second

const (
	maxRequestBodyBytes   = 32 << 20 // largest proxied request body (matches the front server)
	maxInflightRequests   = 256      // most simultaneously-open request ids per guest session
	maxConcurrentPerGrant = 8        // most requests proxied to the upstream at once, per grant
	revokedPresenceTTL    = 7 * 24 * time.Hour
)

// allowedProxyPaths are the only upstream paths a guest may reach through a host.
// /v1/models is deliberately excluded: guests learn the shared model list from
// presence, so there is no reason to let them enumerate the host's full catalog.
var allowedProxyPaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/embeddings":       true,
	"/v1/messages":         true, // Anthropic Messages API (Claude Code)
}

// providerModelHints maps a provider key to substrings that identify its models.
// Best-effort: used only when a grant shares by provider rather than explicit ids.
var providerModelHints = map[string][]string{
	"claude":         {"claude"},
	"codex":          {"gpt", "o1", "o3", "o4", "codex"},
	"gemini":         {"gemini"},
	"qwen":           {"qwen"},
	"kimi":           {"kimi", "moonshot"},
	"github-copilot": {"copilot"},
	"antigravity":    {"gemini", "antigravity"},
	"zai":            {"glm"},
	"xai":            {"grok"},
}

// HostManager runs the host side of every active grant.
type HostManager struct {
	store    *Store
	nostr    *NostrClient
	upstream *Upstream
	subUsage *SubscriptionMonitor // host's own provider session-usage, for the reserve gate
	ctx      context.Context
	notify   func() // called when host-visible state changes (for SSE)
	activity *ActivityLog

	mu     sync.Mutex
	grants map[string]*hostGrant
}

type hostGrant struct {
	grant  Grant
	keys   grantKeys
	ctx    context.Context // cancelled when the grant is revoked/stopped
	cancel context.CancelFunc
	sem    chan struct{} // bounds concurrent proxied requests (maxConcurrentPerGrant)

	mu               sync.Mutex
	sessions         map[string]*hostSession
	allowedModels    []string
	allowAll         bool
	models           []string // advertised list (what guests see)
	shareNote        string   // why the list is fully empty (reserve gate), shown to the guest
	limitedProviders []string // providers currently auto-paused by the reserve gate (partial or full)
	lastGuestSeen    int64
	activeReqs       int
	warnedEmpty      bool // tracks the 0-models warning so we log only on transitions
}

type hostSession struct {
	id string
	pc *webrtc.PeerConnection

	mu     sync.Mutex // guards dc + authed + inbound (written/read from pion callback goroutines)
	dc     *webrtc.DataChannel
	authed bool

	// inbound accumulates chunked request bodies keyed by request id. Guarded
	// by mu: pion runs one OnMessage goroutine per data channel, and a guest
	// can open extra channels on the same session at any time.
	inbound map[string]*inboundReq
}

type inboundReq struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func (hs *hostSession) setChannel(dc *webrtc.DataChannel) {
	hs.mu.Lock()
	hs.dc = dc
	hs.mu.Unlock()
}

func (hs *hostSession) channel() *webrtc.DataChannel {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.dc
}

func (hs *hostSession) markAuthed() {
	hs.mu.Lock()
	hs.authed = true
	hs.mu.Unlock()
}

func (hs *hostSession) isAuthed() bool {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.authed
}

func newHostManager(ctx context.Context, store *Store, nostr *NostrClient, upstream *Upstream, subUsage *SubscriptionMonitor, notify func(), activity *ActivityLog) *HostManager {
	return &HostManager{
		store:    store,
		nostr:    nostr,
		upstream: upstream,
		subUsage: subUsage,
		ctx:      ctx,
		notify:   notify,
		activity: activity,
		grants:   map[string]*hostGrant{},
	}
}

// Reconcile starts workers for newly active grants and stops revoked ones.
func (h *HostManager) Reconcile() {
	active := map[string]Grant{}
	for _, g := range h.store.ActiveGrants() {
		active[g.ID] = g
	}

	h.mu.Lock()
	for id, g := range active {
		if _, ok := h.grants[id]; !ok {
			h.startGrant(g)
		}
	}
	var stopped []*hostGrant
	for id, hg := range h.grants {
		if _, ok := active[id]; !ok {
			delete(h.grants, id)
			stopped = append(stopped, hg)
		}
	}
	h.mu.Unlock()

	for _, hg := range stopped {
		h.stopRevokedGrant(hg)
	}
	h.broadcastRevokedGrants()
}

// RefreshGrant recomputes a grant's shared models and re-broadcasts presence
// immediately (e.g. after pause/resume), instead of waiting for the next tick.
func (h *HostManager) RefreshGrant(id string) {
	h.mu.Lock()
	hg, ok := h.grants[id]
	h.mu.Unlock()
	if !ok {
		return
	}
	go func() {
		h.recomputeModels(hg) // blocking upstream call — never under h.mu
		h.sendPresence(hg)
		h.notify()
	}()
}

// RefreshAll recomputes shared models and re-broadcasts presence for every
// active grant (e.g. after a config change to the session-reserve gate), so the
// new state lands immediately instead of waiting for each grant's presence tick.
func (h *HostManager) RefreshAll() {
	h.mu.Lock()
	grants := make([]*hostGrant, 0, len(h.grants))
	for _, hg := range h.grants {
		grants = append(grants, hg)
	}
	h.mu.Unlock()
	if len(grants) == 0 {
		return
	}
	go func() {
		for _, hg := range grants {
			h.recomputeModels(hg) // blocking upstream call — never under h.mu
			h.sendPresence(hg)
		}
		h.notify()
	}()
}

// caller holds h.mu
func (h *HostManager) startGrant(g Grant) {
	keys, err := deriveGrantKeys(g.Code)
	if err != nil {
		log.Printf("host: bad grant %s: %v", g.ID, err)
		return
	}
	ctx, cancel := context.WithCancel(h.ctx)
	hg := &hostGrant{
		grant:    g,
		keys:     keys,
		ctx:      ctx,
		cancel:   cancel,
		sem:      make(chan struct{}, maxConcurrentPerGrant),
		sessions: map[string]*hostSession{},
	}
	h.grants[g.ID] = hg
	// Model recomputation does a blocking HTTP call to the upstream, so it runs
	// inside presenceLoop — never while holding h.mu (which Reconcile/Status need).

	h.nostr.AddRoom(keys.roomID, func(ev *nostr.Event) { h.onEvent(hg, ev) })
	go h.presenceLoop(ctx, hg)
	log.Printf("host: serving grant %q (room %s)", g.Label, keys.roomID[:8])
}

// grantPaused reports whether a grant is currently paused for the guest — either
// individually (per-grant Pause) or by the global sharing master switch
// (EnableSharing off). A paused grant advertises no models but keeps broadcasting
// presence with Paused=true, so flipping the master switch off shows every friend
// "paused" (like pausing each one) instead of letting them age out to offline.
func (h *HostManager) grantPaused(hg *hostGrant) bool {
	return !h.store.Config().EnableSharing || h.store.GrantPaused(hg.grant.ID)
}

func (h *HostManager) recomputeModels(hg *hostGrant) {
	upstreamModels, _ := h.upstream.ListModels()
	models, allowAll := computeSharedModels(hg.grant, upstreamModels)
	paused := h.grantPaused(hg)
	if paused {
		// Paused: advertise nothing so the guest's /v1/models drops these, but
		// keep presence flowing so the guest sees "paused" rather than offline.
		models, allowAll = nil, false
	}
	// Session-reserve gate: when the host opted to keep a buffer, drop models of
	// any monitored provider whose 5-hour session window is past the threshold.
	// limited lists the providers it paused (partial: some models remain; full:
	// none do) so both the host and the friend can show which ones are limited.
	var limited []string
	if !paused {
		models, allowAll, limited = h.applyUsageReserve(models, allowAll)
	}
	reserved := len(limited) > 0
	// When the reserve gate hides every shared model, the guest would otherwise
	// see an online host it can't use; record why so presence can say "paused".
	note := ""
	if reserved && len(models) == 0 {
		note = "reached their session usage limit — sharing resumes when it resets"
	}
	hg.mu.Lock()
	hg.allowAll = allowAll
	hg.models = models
	hg.allowedModels = models
	hg.shareNote = note
	hg.limitedProviders = limited
	empty := len(models) == 0 && !paused
	transition := empty != hg.warnedEmpty
	hg.warnedEmpty = empty
	label := hg.grant.Label
	hg.mu.Unlock()

	// Surface the most common "nothing routes" cause: the provider engine isn't
	// serving any models (cli-proxy-api down, or no provider connected) — unless
	// the session-reserve gate is what trimmed the list, which is expected.
	if transition {
		switch {
		case empty && reserved:
			log.Printf("host: grant %q is sharing 0 models — session usage is past your reserve buffer, will resume as the window resets", label)
		case empty:
			log.Printf("host: grant %q is sharing 0 models — connect a provider / check cli-proxy-api is running", label)
		default:
			log.Printf("host: grant %q now sharing %d models", label, len(models))
		}
	}
}

// applyUsageReserve removes models belonging to a monitored provider (claude,
// codex) whose 5-hour session window has climbed past 100-reserve%, so the host
// keeps the configured buffer of its own subscription. It fails open: a provider
// with unknown usage (never polled, or its usage endpoint errored) is left
// untouched. When it removes anything it returns allowAll=false so the trimmed
// list — not a blanket allow — is what request-time enforcement consults.
func (h *HostManager) applyUsageReserve(models []string, allowAll bool) (out []string, gatedAllowAll bool, limited []string) {
	cfg := h.store.Config()
	if !cfg.AutoStopSharing || cfg.UsageReservePercent <= 0 || cfg.UsageReservePercent >= 100 {
		return models, allowAll, nil
	}
	if h.subUsage == nil {
		return models, allowAll, nil
	}
	threshold := 100 - float64(cfg.UsageReservePercent)
	blocked := map[string]bool{}
	for _, p := range monitoredUsageProviders {
		if util, known := h.subUsage.SessionUtilization(p); known && util >= threshold {
			blocked[p] = true
		}
	}
	if len(blocked) == 0 {
		return models, allowAll, nil
	}
	kept := make([]string, 0, len(models))
	removedProviders := map[string]bool{}
	for _, m := range models {
		if p := monitoredProviderForModel(m); p != "" && blocked[p] {
			removedProviders[p] = true
			continue
		}
		kept = append(kept, m)
	}
	if len(removedProviders) == 0 {
		return models, allowAll, nil // a blocked provider, but it shared no models here
	}
	for _, p := range monitoredUsageProviders { // stable, human order
		if removedProviders[p] {
			limited = append(limited, providerDisplayName(p))
		}
	}
	return kept, false, limited
}

// usageShare builds the subscription disclosure that rides along with presence,
// clamped to whatever level the host chose:
//
//	off      nothing — the guest still sees LimitedProviders, as it always has
//	resets   only the reset time of a provider ALREADY disclosed as limited, so
//	         "Codex paused" gains a "back in 40m" and nothing else
//	windows  the live session percentage for every monitored provider behind
//	         this grant, limited or not — the only level that warns a friend
//	         before a squeeze rather than after it costs them a request
//
// `limited` is what the reserve gate paused; `models` is what this grant is
// actually advertising. Together they bound disclosure to providers this friend
// can see anyway: a grant sharing only Claude never learns about Codex.
func (h *HostManager) usageShare(limited, models []string) []providerUsageShare {
	if h.subUsage == nil {
		return nil
	}
	level := h.store.Config().shareUsageLevel()
	if level == shareUsageOff {
		return nil
	}
	isLimited := make(map[string]bool, len(limited))
	for _, name := range limited {
		isLimited[name] = true
	}
	// A limited provider's models are gone from `models` (the gate removed them),
	// so relevance is the union: still serving here, or paused out of here.
	relevant := map[string]bool{}
	for _, m := range models {
		if p := monitoredProviderForModel(m); p != "" {
			relevant[p] = true
		}
	}
	var out []providerUsageShare
	for _, p := range monitoredUsageProviders { // stable, human order
		name := providerDisplayName(p)
		if !relevant[p] && !isLimited[name] {
			continue
		}
		// At "resets" the disclosure is strictly a timestamp on a pause the guest
		// is already told about, so an unlimited provider contributes nothing.
		if level == shareUsageResets && !isLimited[name] {
			continue
		}
		row := providerUsageShare{Provider: name, Limited: isLimited[name], ResetsAt: h.subUsage.SessionResetsAt(p)}
		if level == shareUsageWindows {
			if util, known := h.subUsage.SessionUtilization(p); known {
				row.Utilization = &util
			}
		}
		// Nothing known about this provider yet: don't announce an empty row.
		if row.ResetsAt == "" && row.Utilization == nil {
			continue
		}
		out = append(out, row)
	}
	return out
}

// providerDisplayName turns a provider key into the label shown in the UI.
func providerDisplayName(key string) string {
	switch key {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "":
		return ""
	default:
		return strings.ToUpper(key[:1]) + key[1:]
	}
}

// monitoredUsageProviders are the providers SubscriptionMonitor can read a live
// session window for, and thus the only ones the reserve gate can act on.
var monitoredUsageProviders = []string{"claude", "codex"}

// monitoredProviderForModel returns the monitored provider ("claude"/"codex")
// a model belongs to, or "" if it is served by an unmonitored provider.
func monitoredProviderForModel(modelID string) string {
	lid := strings.ToLower(modelID)
	for _, p := range monitoredUsageProviders {
		for _, hint := range providerModelHints[p] {
			if strings.Contains(lid, hint) {
				return p
			}
		}
	}
	return ""
}

func computeSharedModels(g Grant, upstream []modelObject) (models []string, allowAll bool) {
	available := map[string]bool{}
	var availIDs []string
	for _, m := range upstream {
		available[m.ID] = true
		availIDs = append(availIDs, m.ID)
	}

	// Explicit model list wins.
	if len(g.Models) > 0 {
		if len(available) == 0 {
			return append([]string(nil), g.Models...), false
		}
		var out []string
		for _, m := range g.Models {
			if available[m] {
				out = append(out, m)
			}
		}
		return out, false
	}

	// Provider filter.
	if len(g.Providers) > 0 {
		var out []string
		for _, id := range availIDs {
			if matchesAnyProvider(id, g.Providers) {
				out = append(out, id)
			}
		}
		return out, false
	}

	// Nothing specified: share everything available.
	return availIDs, true
}

func matchesAnyProvider(modelID string, providers []string) bool {
	lid := strings.ToLower(modelID)
	for _, p := range providers {
		for _, hint := range providerModelHints[strings.ToLower(p)] {
			if strings.Contains(lid, hint) {
				return true
			}
		}
	}
	return false
}

func (h *HostManager) presenceLoop(ctx context.Context, hg *hostGrant) {
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	h.recomputeModels(hg)
	h.sendPresence(hg)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.recomputeModels(hg)
			h.sendPresence(hg)
		}
	}
}

// buildPresence assembles the host presence a guest reads. When the grant is
// paused (individually or via the master switch) it carries no models and
// Paused=true, so the guest shows "paused" rather than aging out to "offline".
func (h *HostManager) buildPresence(hg *hostGrant) presenceContent {
	switched := h.grantPaused(hg) // master switch off, or this grant paused
	hg.mu.Lock()
	models := append([]string(nil), hg.models...)
	note := hg.shareNote
	limited := append([]string(nil), hg.limitedProviders...)
	hg.mu.Unlock()
	if switched {
		models = nil
	}
	// A grant advertising no models can't serve anything, so present it as paused
	// — otherwise the friend sees a green "online" host they can't actually use
	// (e.g. the reserve gate hid every model). Reason explains why, except for a
	// deliberate pause, which the guest UI already labels on its own.
	paused := switched || len(models) == 0
	reason := ""
	if paused && !switched {
		if reason = note; reason == "" {
			reason = "isn't sharing any models right now"
		}
	}
	u := h.store.Usage(hg.grant.ID)
	return presenceContent{
		Role:             "host",
		Name:             h.store.Config().IdentityName,
		Models:           models,
		Paused:           paused,
		Reason:           reason,
		LimitedProviders: limited,
		Usage:            h.usageShare(limited, models),
		TokenLimit:       h.store.GrantLimit(hg.grant.ID),
		TokensUsed:       u.InputTokens + u.OutputTokens,
		TS:               time.Now().Unix(),
	}
}

func (h *HostManager) sendPresence(hg *hostGrant) {
	// Always broadcast (even with the master switch off): going silent would age
	// the guest out to "offline" with no reason shown; a paused presence doesn't.
	content, err := sealJSON(hg.keys.announceKey, h.buildPresence(hg))
	if err != nil {
		return
	}
	h.nostr.Publish(hg.keys.roomID, "presence", content)
}

func (h *HostManager) stopRevokedGrant(hg *hostGrant) {
	h.sendRevokedPresence(hg)
	hg.cancel()
	h.closeAllSessions(hg)
	h.nostr.RemoveRoom(hg.keys.roomID)
}

func (h *HostManager) sendRevokedPresence(hg *hostGrant) {
	h.publishRevokedPresence(hg.keys, 3)
}

func (h *HostManager) broadcastRevokedGrants() {
	now := time.Now()
	for _, g := range h.store.Grants() {
		if !g.Revoked || g.RevokedAt == 0 {
			continue
		}
		if now.Sub(time.Unix(g.RevokedAt, 0)) > revokedPresenceTTL {
			continue
		}
		keys, err := deriveGrantKeys(g.Code)
		if err != nil {
			continue
		}
		h.publishRevokedPresence(keys, 1)
	}
}

func (h *HostManager) publishRevokedPresence(keys grantKeys, count int) {
	for i := 0; i < count; i++ {
		content, err := sealJSON(keys.announceKey, presenceContent{
			Role:    "host",
			Name:    h.store.Config().IdentityName,
			Revoked: true,
			Paused:  true,
			Reason:  "revoked this share",
			TS:      time.Now().Unix(),
		})
		if err != nil {
			return
		}
		h.nostr.Publish(keys.roomID, "presence", content)
	}
}

func (h *HostManager) onEvent(hg *hostGrant, ev *nostr.Event) {
	switch tagValue(ev, "t") {
	case "presence":
		var pc presenceContent
		if !openJSON(hg.keys.announceKey, ev.Content, &pc) {
			return
		}
		if pc.Role == "guest" && freshTimestamp(pc.TS, authWindow) {
			hg.mu.Lock()
			hg.lastGuestSeen = time.Now().Unix()
			hg.mu.Unlock()
			h.notify()
		}
	case "signal":
		var sc signalContent
		if !openJSON(hg.keys.announceKey, ev.Content, &sc) {
			return
		}
		switch sc.Kind {
		case "offer":
			h.handleOffer(hg, sc)
		case "ice":
			h.handleICE(hg, sc)
		}
	}
}

func (h *HostManager) handleOffer(hg *hostGrant, sc signalContent) {
	var offer webrtc.SessionDescription
	if err := json.Unmarshal(sc.Payload, &offer); err != nil {
		return
	}
	session := sc.Session
	if session == "" {
		session = sc.From
	}

	hg.mu.Lock()
	if _, exists := hg.sessions[session]; exists {
		hg.mu.Unlock()
		return // duplicate offer (seen via another relay)
	}
	pc, err := webrtc.NewPeerConnection(webrtcConfig())
	if err != nil {
		hg.mu.Unlock()
		return
	}
	hs := &hostSession{id: session, pc: pc, inbound: map[string]*inboundReq{}}
	hg.sessions[session] = hs
	hg.mu.Unlock()
	log.Printf("host[%s]: offer received", session)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cand, _ := json.Marshal(c.ToJSON())
		h.sendSignal(hg, session, "ice", cand)
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("host[%s]: ICE state -> %s", session, state)
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("host[%s]: conn state -> %s", session, s)
		if s == webrtc.PeerConnectionStateFailed ||
			s == webrtc.PeerConnectionStateClosed ||
			s == webrtc.PeerConnectionStateDisconnected {
			h.closeSession(hg, session)
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("host[%s]: data channel established", session)
		hs.setChannel(dc)
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			h.onFrame(hg, hs, msg.Data)
		})
		dc.OnClose(func() { h.closeSession(hg, session) })
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		h.closeSession(hg, session)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		h.closeSession(hg, session)
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		h.closeSession(hg, session)
		return
	}
	ansJSON, _ := json.Marshal(answer)
	h.sendSignal(hg, session, "answer", ansJSON)
	log.Printf("host[%s]: answer sent", session)
}

func (h *HostManager) handleICE(hg *hostGrant, sc signalContent) {
	hg.mu.Lock()
	hs, ok := hg.sessions[sc.Session]
	hg.mu.Unlock()
	if !ok || hs.pc == nil {
		return
	}
	var cand webrtc.ICECandidateInit
	if err := json.Unmarshal(sc.Payload, &cand); err != nil {
		return
	}
	_ = hs.pc.AddICECandidate(cand)
}

func (h *HostManager) sendSignal(hg *hostGrant, session, kind string, payload json.RawMessage) {
	content, err := sealJSON(hg.keys.announceKey, signalContent{
		Session: session,
		Kind:    kind,
		Payload: payload,
	})
	if err != nil {
		return
	}
	h.nostr.Publish(hg.keys.roomID, "signal", content)
}

func (h *HostManager) closeSession(hg *hostGrant, session string) {
	hg.mu.Lock()
	hs, ok := hg.sessions[session]
	if ok {
		delete(hg.sessions, session)
	}
	hg.mu.Unlock()
	if ok && hs.pc != nil {
		_ = hs.pc.Close()
	}
}

func (h *HostManager) closeAllSessions(hg *hostGrant) {
	hg.mu.Lock()
	sessions := make([]*hostSession, 0, len(hg.sessions))
	for id, hs := range hg.sessions {
		delete(hg.sessions, id)
		sessions = append(sessions, hs)
	}
	hg.mu.Unlock()
	for _, hs := range sessions {
		if hs.pc != nil {
			_ = hs.pc.Close()
		}
	}
}

// grantActive reports whether a grant may accept NEW requests. False the instant the
// grant is revoked (Reconcile cancels its ctx) or sharing is paused. It deliberately
// does NOT cancel in-flight requests — proxyRequest streams off h.ctx — so a response
// already under way completes, while the next request from a revoked/paused guest is
// refused even on an already-open data channel.
func (h *HostManager) grantActive(hg *hostGrant) bool {
	if hg.ctx == nil || hg.ctx.Err() != nil {
		return false
	}
	return h.store.Config().EnableSharing
}

func (h *HostManager) onFrame(hg *hostGrant, hs *hostSession, data []byte) {
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	dc := hs.channel()
	if dc == nil {
		return
	}
	switch f.T {
	case "auth":
		var ap authPayload
		if !openJSON(hg.keys.channelKey, f.Payload, &ap) || !freshTimestamp(ap.TS, authWindow) {
			_ = sendFrame(dc, frame{T: "auth_err", Msg: "unauthorized"})
			return
		}
		hs.markAuthed()
		hg.mu.Lock()
		hg.lastGuestSeen = time.Now().Unix()
		hg.mu.Unlock()
		_ = sendFrame(dc, frame{T: "auth_ok"})
		h.notify()
	case "req":
		if !hs.isAuthed() {
			_ = sendFrame(dc, frame{T: "err", ID: f.ID, Msg: "not authenticated"})
			return
		}
		if !h.grantActive(hg) {
			_ = sendFrame(dc, frame{T: "err", ID: f.ID, Msg: "grant revoked or sharing paused"})
			return
		}
		hs.mu.Lock()
		if hs.inbound == nil {
			hs.inbound = map[string]*inboundReq{}
		}
		if len(hs.inbound) >= maxInflightRequests {
			hs.mu.Unlock()
			_ = sendFrame(dc, frame{T: "err", ID: f.ID, Msg: "too many in-flight requests"})
			return
		}
		hs.inbound[f.ID] = &inboundReq{method: f.Method, path: f.Path, header: cloneForwardHeaders(f.Headers)}
		hs.mu.Unlock()
	case "reqdata":
		chunk, err := base64.StdEncoding.DecodeString(f.B64)
		if err != nil {
			return
		}
		hs.mu.Lock()
		ir := hs.inbound[f.ID]
		if ir == nil {
			hs.mu.Unlock()
			return
		}
		if len(ir.body)+len(chunk) > maxRequestBodyBytes {
			delete(hs.inbound, f.ID)
			hs.mu.Unlock()
			_ = sendFrame(dc, frame{T: "err", ID: f.ID, Msg: "request body too large"})
			return
		}
		ir.body = append(ir.body, chunk...)
		hs.mu.Unlock()
	case "reqend":
		hs.mu.Lock()
		ir := hs.inbound[f.ID]
		delete(hs.inbound, f.ID)
		hs.mu.Unlock()
		if ir == nil {
			return
		}
		select {
		case hg.sem <- struct{}{}:
			go h.proxyRequest(hg, hs, f.ID, ir.method, ir.path, ir.header, ir.body)
		default:
			_ = sendFrame(dc, frame{T: "err", ID: f.ID, Msg: "host busy: too many concurrent requests, retry shortly"})
		}
	}
}

func (h *HostManager) proxyRequest(hg *hostGrant, hs *hostSession, id, method, path string, header http.Header, body []byte) {
	defer func() { <-hg.sem }() // release the slot acquired in onFrame's "reqend"
	dc := hs.channel()
	if dc == nil {
		return
	}
	if !h.grantActive(hg) {
		_ = sendFrame(dc, frame{T: "err", ID: id, Msg: "grant revoked or sharing paused"})
		return
	}
	// Every allowed path is a completion-style POST carrying a model; parse it
	// up front so refusals can be recorded in the activity feed too.
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	started := time.Now()
	refuse := func(msg string) {
		_ = sendFrame(dc, frame{T: "err", ID: id, Msg: msg})
		h.activity.Add(ActivityEntry{
			TS: started.Unix(), Direction: "hosted", Peer: hg.grant.Label,
			Model: probe.Model, Status: "error", Error: msg,
		})
	}

	if !allowedProxyPaths[path] {
		refuse("path not allowed")
		return
	}

	// Enforce the grant's allow-list before touching the upstream.
	if h.store.GrantPaused(hg.grant.ID) {
		refuse("sharing is paused — ask your friend to resume it in VibeShare")
		return
	}
	if !h.grantAllowsModel(hg, probe.Model) {
		refuse("model not shared: " + probe.Model)
		return
	}

	// Enforce the friend's token allotment before spending any of the host's
	// subscription. Read live from the store so top-ups apply immediately. This
	// is a soft cap: a request already in flight may carry the tally slightly
	// past the limit, but the next one is refused.
	if limit := h.store.GrantLimit(hg.grant.ID); limit > 0 {
		u := h.store.Usage(hg.grant.ID)
		if u.InputTokens+u.OutputTokens >= limit {
			refuse("token allotment exhausted — ask your friend to top it up")
			return
		}
	}

	if method == "" {
		method = http.MethodPost
	}

	hg.mu.Lock()
	hg.activeReqs++
	hg.mu.Unlock()
	h.notify()
	defer func() {
		hg.mu.Lock()
		hg.activeReqs--
		hg.mu.Unlock()
		h.notify()
	}()

	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()

	resp, err := h.upstream.doWithHeaders(ctx, method, path, body, header)
	if err != nil {
		refuse("upstream error: " + err.Error())
		return
	}
	defer resp.Body.Close()

	// The request reached the provider — count it and tally its tokens (parsed
	// from the response as it streams past), persisted so it survives restarts.
	// The same numbers feed the activity entry for the live feed.
	tracker := newUsageTracker(resp.Header.Get("Content-Type"))
	streamErr := ""
	defer func() {
		in, out := tracker.Finish()
		h.store.AddUsage(hg.grant.ID, 1, in, out)
		status, errMsg := "ok", streamErr
		if errMsg == "" && resp.StatusCode >= 400 {
			errMsg = "provider returned HTTP " + strconv.Itoa(resp.StatusCode)
		}
		if errMsg != "" {
			status = "error"
		}
		h.activity.Add(ActivityEntry{
			TS: started.Unix(), Direction: "hosted", Peer: hg.grant.Label,
			Model: probe.Model, Status: status, Error: errMsg,
			InputTokens: in, OutputTokens: out,
			DurationMs: time.Since(started).Milliseconds(),
		})
		h.notify()
	}()

	if err := sendFrame(dc, frame{
		T:      "head",
		ID:     id,
		Status: resp.StatusCode,
		Ctype:  resp.Header.Get("Content-Type"),
	}); err != nil {
		streamErr = "data channel write failed: " + err.Error()
		return
	}

	buf := make([]byte, 8*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			tracker.Write(buf[:n])
			if err := sendFrame(dc, frame{
				T:   "data",
				ID:  id,
				B64: base64.StdEncoding.EncodeToString(buf[:n]),
			}); err != nil {
				streamErr = "data channel write failed: " + err.Error()
				return
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				_ = sendFrame(dc, frame{T: "err", ID: id, Msg: readErr.Error()})
				streamErr = readErr.Error()
				return
			}
			break
		}
	}
	if err := sendFrame(dc, frame{T: "end", ID: id}); err != nil {
		streamErr = "data channel write failed: " + err.Error()
	}
}

func (h *HostManager) grantAllowsModel(hg *hostGrant, model string) bool {
	hg.mu.Lock()
	defer hg.mu.Unlock()
	if hg.allowAll {
		return true
	}
	for _, m := range hg.allowedModels {
		if m == model {
			return true
		}
	}
	return false
}

// GrantStatus is the host-side runtime view of one grant, for the control API.
type GrantStatus struct {
	Online           bool     `json:"online"`
	Routing          bool     `json:"routing"`
	Models           []string `json:"models"`
	UsageLimited     bool     `json:"usageLimited"`     // reserve gate hid ALL of this grant's models (fully auto-paused)
	LimitedProviders []string `json:"limitedProviders"` // providers the reserve gate paused (partial or full)
	TotalReqs        int      `json:"totalReqs"`
	InputTokens      int64    `json:"inputTokens"`
	OutputTokens     int64    `json:"outputTokens"`
}

func (h *HostManager) Status(grantID string) GrantStatus {
	// Usage is persisted, so it is reported even for a stopped/revoked grant.
	u := h.store.Usage(grantID)
	st := GrantStatus{
		TotalReqs:    int(u.Requests),
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
	}

	h.mu.Lock()
	hg, ok := h.grants[grantID]
	h.mu.Unlock()
	if !ok {
		return st
	}
	hg.mu.Lock()
	defer hg.mu.Unlock()
	online := time.Now().Unix()-hg.lastGuestSeen < 60
	for _, s := range hg.sessions {
		if s.isAuthed() {
			online = true
		}
	}
	st.Online = online
	st.Routing = hg.activeReqs > 0
	st.Models = append([]string(nil), hg.models...)
	st.UsageLimited = hg.shareNote != "" // reserve gate emptied ALL shared models
	st.LimitedProviders = append([]string(nil), hg.limitedProviders...)
	return st
}

func tagValue(ev *nostr.Event, name string) string {
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == name {
			return t[1]
		}
	}
	return ""
}
