package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/pion/webrtc/v3"
)

// GuestManager runs the guest side of every held connection.
type GuestManager struct {
	store    *Store
	nostr    *NostrClient
	ctx      context.Context
	notify   func()
	activity *ActivityLog

	mu       sync.Mutex
	conns    map[string]*guestConn
	lastHost map[string]string // model -> connection ID that last served it (sticky routing)
}

type guestConn struct {
	conn   Connection
	keys   grantKeys
	peerID string
	cancel context.CancelFunc

	mu          sync.Mutex
	hostName    string
	hostModels  []string
	hostRevoked bool                 // host permanently revoked this grant
	hostPaused  bool                 // host paused sharing (still online, advertising 0 models)
	hostReason  string               // why the host is fully paused (e.g. session usage limit), if given
	hostLimited []string             // providers the host auto-paused by reserve (partial or full)
	hostUsage   []providerUsageShare // host's session windows, as far as they opted to share
	tokenLimit  int64                // host-advertised allotment for this connection (0 = unlimited)
	tokensUsed  int64                // host-authoritative tokens consumed so far
	lastSeen    int64
	active      int
	session     *guestSession
}

type guestSession struct {
	id       string
	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
	keys     grantKeys
	authOK   chan struct{}
	authErr  string
	authOnce sync.Once
	closed   chan struct{}
	closeMu  sync.Once

	mu      sync.Mutex
	pending map[string]*pendingReq
}

// pendingReq plumbs a single in-flight request's response frames from the pion
// callback goroutine to the RouteRequest goroutine. `done` lets deliver() stop
// blocking the moment the request ends, so frames are never dropped (which would
// corrupt a stream) and never sent on an abandoned channel.
type pendingReq struct {
	ch   chan respEvent
	done chan struct{}
}

type respEvent struct {
	kind   string // head | data | end | err
	status int
	ctype  string
	data   []byte
	msg    string
}

func newGuestManager(ctx context.Context, store *Store, nostr *NostrClient, notify func(), activity *ActivityLog) *GuestManager {
	return &GuestManager{
		store:    store,
		nostr:    nostr,
		ctx:      ctx,
		notify:   notify,
		activity: activity,
		conns:    map[string]*guestConn{},
		lastHost: map[string]string{},
	}
}

func (g *GuestManager) Reconcile() {
	want := map[string]Connection{}
	for _, c := range g.store.Connections() {
		if c.Revoked {
			continue
		}
		want[c.ID] = c
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	for id, c := range want {
		if _, ok := g.conns[id]; !ok {
			g.startConn(c)
		}
	}
	for id, gc := range g.conns {
		if _, ok := want[id]; !ok {
			gc.cancel()
			g.nostr.RemoveRoom(gc.keys.roomID)
			delete(g.conns, id)
		}
	}
}

// caller holds g.mu
func (g *GuestManager) startConn(c Connection) {
	keys, err := deriveGrantKeys(c.Code)
	if err != nil {
		log.Printf("guest: bad connection %s: %v", c.ID, err)
		return
	}
	ctx, cancel := context.WithCancel(g.ctx)
	gc := &guestConn{
		conn:   c,
		keys:   keys,
		peerID: randomID(6),
		cancel: cancel,
	}
	g.conns[c.ID] = gc
	g.nostr.AddRoom(keys.roomID, func(ev *nostr.Event) { g.onEvent(gc, ev) })
	go g.presenceLoop(ctx, gc)
	log.Printf("guest: connected to %q (room %s)", c.Label, keys.roomID[:8])
}

func (g *GuestManager) presenceLoop(ctx context.Context, gc *guestConn) {
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	g.sendPresence(gc)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			g.sendPresence(gc)
		}
	}
}

func (g *GuestManager) sendPresence(gc *guestConn) {
	if !g.store.Config().EnableSharing {
		return
	}
	gc.mu.Lock()
	if gc.hostRevoked {
		gc.mu.Unlock()
		return
	}
	routing := gc.active > 0
	gc.mu.Unlock()
	content, err := sealJSON(gc.keys.announceKey, presenceContent{
		Role:    "guest",
		Peer:    gc.peerID,
		Routing: routing,
		TS:      time.Now().Unix(),
	})
	if err != nil {
		return
	}
	g.nostr.Publish(gc.keys.roomID, "presence", content)
}

func (g *GuestManager) onEvent(gc *guestConn, ev *nostr.Event) {
	switch tagValue(ev, "t") {
	case "presence":
		var pc presenceContent
		if !openJSON(gc.keys.announceKey, ev.Content, &pc) || pc.Role != "host" {
			return
		}
		if !freshTimestamp(pc.TS, authWindow) {
			return // stale/replayed presence
		}
		gc.mu.Lock()
		gc.hostName = pc.Name
		gc.hostRevoked = pc.Revoked
		if pc.Revoked {
			gc.hostModels = nil
			gc.hostPaused = true
		} else {
			gc.hostModels = pc.Models
			gc.hostPaused = pc.Paused
		}
		gc.hostReason = pc.Reason
		gc.hostLimited = pc.LimitedProviders
		gc.hostUsage = append([]providerUsageShare(nil), pc.Usage...)
		gc.tokenLimit = pc.TokenLimit
		gc.tokensUsed = pc.TokensUsed
		if pc.Revoked {
			gc.lastSeen = 0
		} else {
			gc.lastSeen = time.Now().Unix()
		}
		gc.mu.Unlock()
		if pc.Revoked {
			_ = g.store.MarkConnectionRevoked(gc.conn.ID)
			go g.Reconcile()
		}
		g.notify()
	case "signal":
		var sc signalContent
		if !openJSON(gc.keys.announceKey, ev.Content, &sc) {
			return
		}
		gc.mu.Lock()
		s := gc.session
		gc.mu.Unlock()
		if s == nil || s.id != sc.Session {
			return
		}
		switch sc.Kind {
		case "answer":
			log.Printf("guest[%s]: answer received", s.id)
			var answer webrtc.SessionDescription
			if json.Unmarshal(sc.Payload, &answer) == nil {
				_ = s.pc.SetRemoteDescription(answer)
			}
		case "ice":
			var cand webrtc.ICECandidateInit
			if json.Unmarshal(sc.Payload, &cand) == nil {
				_ = s.pc.AddICECandidate(cand)
			}
		}
	}
}

// RemoteModel is an advertised model from an online host.
type RemoteModel struct {
	ID    string
	Owner string
}

// RemoteModels lists models reachable through currently-online hosts.
func (g *GuestManager) RemoteModels() []RemoteModel {
	g.mu.Lock()
	conns := make([]*guestConn, 0, len(g.conns))
	for _, gc := range g.conns {
		conns = append(conns, gc)
	}
	g.mu.Unlock()
	sortGuestConns(conns)

	var out []RemoteModel
	seen := map[string]bool{}
	for _, gc := range conns {
		gc.mu.Lock()
		online := time.Now().Unix()-gc.lastSeen < 60
		revoked := gc.hostRevoked
		name := gc.hostName
		models := append([]string(nil), gc.hostModels...)
		gc.mu.Unlock()
		if revoked || !online {
			continue
		}
		for _, m := range models {
			if seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, RemoteModel{ID: m, Owner: name})
		}
	}
	return out
}

// hostsForModel returns the online hosts advertising the model, best first:
//
//  1. the host that last successfully served this model (sticky — keeps the
//     provider-side prompt cache warm across an agentic session),
//  2. then hosts with budget left, most remaining allotment first (unlimited
//     outranks any finite budget),
//  3. exhausted allotments last — still tried as a last resort so the host's
//     clear "allotment exhausted" refusal reaches the client when nobody else
//     can serve.
//
// Replaces the old first-match-in-map-order pick, which was effectively random
// per request when several friends shared the same model.
func (g *GuestManager) hostsForModel(model string) []*guestConn {
	g.mu.Lock()
	sticky := g.lastHost[model]
	conns := make([]*guestConn, 0, len(g.conns))
	for _, gc := range g.conns {
		conns = append(conns, gc)
	}
	g.mu.Unlock()

	type cand struct {
		gc        *guestConn
		bucket    int   // 0 sticky · 1 budget left · 2 exhausted
		remaining int64 // -1 = unlimited
		label     string
		id        string
	}
	var cands []cand
	for _, gc := range conns {
		gc.mu.Lock()
		online := time.Now().Unix()-gc.lastSeen < 60
		revoked := gc.hostRevoked
		has := false
		for _, m := range gc.hostModels {
			if m == model {
				has = true
				break
			}
		}
		exhausted := gc.tokenLimit > 0 && gc.tokensUsed >= gc.tokenLimit
		remaining := int64(-1)
		if gc.tokenLimit > 0 {
			remaining = gc.tokenLimit - gc.tokensUsed
		}
		id := gc.conn.ID
		label := gc.hostName
		if label == "" {
			label = gc.conn.Label
		}
		gc.mu.Unlock()
		if revoked || !online || !has {
			continue
		}
		bucket := 1
		switch {
		case exhausted:
			bucket = 2
		case id == sticky:
			bucket = 0
		}
		cands = append(cands, cand{gc: gc, bucket: bucket, remaining: remaining, label: label, id: id})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].bucket != cands[j].bucket {
			return cands[i].bucket < cands[j].bucket
		}
		ri, rj := cands[i].remaining, cands[j].remaining
		if ri == -1 || rj == -1 {
			if ri != rj {
				return ri == -1 // unlimited first
			}
		} else if ri != rj {
			return ri > rj // then most remaining
		}
		if cands[i].label != cands[j].label {
			return cands[i].label < cands[j].label
		}
		return cands[i].id < cands[j].id
	})
	out := make([]*guestConn, len(cands))
	for i, c := range cands {
		out[i] = c.gc
	}
	return out
}

func sortGuestConns(conns []*guestConn) {
	sort.SliceStable(conns, func(i, j int) bool {
		li, lj := connSortLabel(conns[i]), connSortLabel(conns[j])
		if li != lj {
			return li < lj
		}
		return conns[i].conn.ID < conns[j].conn.ID
	})
}

func connSortLabel(gc *guestConn) string {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if gc.hostName != "" {
		return gc.hostName
	}
	return gc.conn.Label
}

// rememberHost pins future requests for model to the connection that just
// served it successfully.
func (g *GuestManager) rememberHost(model, connID string) {
	g.mu.Lock()
	g.lastHost[model] = connID
	g.mu.Unlock()
}

// connLabel is the friendly name for a connection (presence name, else label).
func connLabel(gc *guestConn) string {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if gc.hostName != "" {
		return gc.hostName
	}
	return gc.conn.Label
}

// CanRoute reports whether some online host advertises the model.
func (g *GuestManager) CanRoute(model string) bool {
	return len(g.hostsForModel(model)) > 0
}

// CanRouteWithBudget reports whether some online host advertises the model and
// is not known to be out of its per-connection allotment. It is used only when
// deciding whether to bypass a known-exhausted local Claude/Codex session.
func (g *GuestManager) CanRouteWithBudget(model string) bool {
	for _, gc := range g.hostsForModel(model) {
		gc.mu.Lock()
		ok := gc.tokenLimit == 0 || gc.tokensUsed < gc.tokenLimit
		gc.mu.Unlock()
		if ok {
			return true
		}
	}
	return false
}

// OnlineFriends reports how many friend connections are online, and how many of
// those are advertising at least one model — used for clearer routing errors.
func (g *GuestManager) OnlineFriends() (online, sharing int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, gc := range g.conns {
		gc.mu.Lock()
		isOnline := time.Now().Unix()-gc.lastSeen < 60
		revoked := gc.hostRevoked
		hasModels := len(gc.hostModels) > 0
		gc.mu.Unlock()
		if isOnline && !revoked {
			online++
			if hasModels {
				sharing++
			}
		}
	}
	return
}

// maxRouteAttempts bounds failover so a request can't crawl through a long
// friend list (each connect attempt can take up to ~25s).
const maxRouteAttempts = 3

// RouteRequest proxies an OpenAI request to an online host over WebRTC,
// streaming the response into w. When several friends share the model it tries
// them best-first (see hostsForModel) and fails over to the next one as long
// as nothing has been written to the client yet.
func (g *GuestManager) RouteRequest(ctx context.Context, model, method, path string, headers http.Header, body []byte, w http.ResponseWriter) error {
	hosts := g.hostsForModel(model)
	if len(hosts) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, errBody("no online friend shares model "+model))
		return errors.New("no online friend shares model " + model)
	}
	if len(hosts) > maxRouteAttempts {
		hosts = hosts[:maxRouteAttempts]
	}

	var lastErr error
	for i, gc := range hosts {
		committed, err := g.routeVia(ctx, gc, model, method, path, headers, body, w)
		if err == nil {
			return nil
		}
		lastErr = err
		if committed || ctx.Err() != nil {
			// Bytes already reached the client (or it went away) — too late to
			// fail over; the error is only for the log/activity feed.
			return err
		}
		if i+1 < len(hosts) {
			log.Printf("guest: %s via %q failed before a response (%v) — trying next friend",
				model, connLabel(gc), err)
		}
	}
	writeJSON(w, http.StatusBadGateway,
		errBody("could not reach "+model+" through any friend: "+lastErr.Error()))
	return lastErr
}

// routeVia attempts the request through one host. committed reports whether
// response bytes (status/body) were already written to w — once true the caller
// must not retry elsewhere. Every attempt that targets a host lands in the
// activity feed with its outcome; usage is only tallied once a response starts.
func (g *GuestManager) routeVia(ctx context.Context, gc *guestConn, model, method, path string, headers http.Header, body []byte, w http.ResponseWriter) (committed bool, retErr error) {
	started := time.Now()
	var tracker *usageTracker
	defer func() {
		var in, out int64
		if tracker != nil {
			in, out = tracker.Finish()
			g.store.AddUsage(gc.conn.ID, 1, in, out)
		}
		status, msg := "ok", ""
		if retErr != nil {
			status, msg = "error", retErr.Error()
		}
		g.activity.Add(ActivityEntry{
			TS: started.Unix(), Direction: "borrowed", Peer: connLabel(gc),
			Model: model, Status: status, Error: msg,
			InputTokens: in, OutputTokens: out,
			DurationMs: time.Since(started).Milliseconds(),
		})
		g.notify()
	}()

	s, err := g.ensureSession(ctx, gc)
	if err != nil {
		return false, errors.New("could not connect to friend: " + err.Error())
	}

	id := randomID(8)
	pr := &pendingReq{ch: make(chan respEvent, 64), done: make(chan struct{})}
	s.mu.Lock()
	s.pending[id] = pr
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		close(pr.done) // unblock any in-flight deliver()
	}()

	gc.mu.Lock()
	gc.active++
	gc.mu.Unlock()
	g.notify()
	defer func() {
		gc.mu.Lock()
		gc.active--
		gc.mu.Unlock()
		g.notify()
	}()

	if err := g.sendRequest(s, id, method, path, headers, body); err != nil {
		return false, errors.New("failed to send request to friend: " + err.Error())
	}

	flusher, _ := w.(http.Flusher)
	wroteHead := false
	headStatus := 0
	// Abort if the host goes silent (covers a wedged peer the client keeps open).
	idle := time.NewTimer(120 * time.Second)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return wroteHead, ctx.Err()
		case <-s.closed:
			return wroteHead, errors.New("connection to friend closed before a response")
		case <-idle.C:
			return wroteHead, errors.New("friend did not respond in time")
		case ev := <-pr.ch:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(120 * time.Second)
			switch ev.kind {
			case "head":
				if ev.ctype != "" {
					w.Header().Set("Content-Type", ev.ctype)
				}
				tracker = newUsageTracker(ev.ctype)
				headStatus = ev.status
				if headStatus == 0 {
					headStatus = http.StatusOK
				}
				w.WriteHeader(headStatus)
				wroteHead = true
			case "data":
				if !wroteHead {
					headStatus = http.StatusOK
					w.WriteHeader(headStatus)
					wroteHead = true
				}
				if tracker != nil {
					tracker.Write(ev.data)
				}
				if _, err := w.Write(ev.data); err != nil {
					return true, err
				}
				if flusher != nil {
					flusher.Flush()
				}
			case "end":
				if headStatus >= 400 {
					// Streamed through, but the host's provider errored — don't
					// pin stickiness to a failing host, and mark the attempt.
					return true, errors.New("friend's provider returned HTTP " + strconv.Itoa(headStatus))
				}
				g.rememberHost(model, gc.conn.ID)
				return true, nil
			case "err":
				// Host refusal (paused / model not shared / allotment exhausted)
				// or upstream failure. Before any bytes it's safe to fail over.
				return wroteHead, errors.New(ev.msg)
			}
		}
	}
}

// sendRequest streams the request to the host as a `req` frame followed by
// base64 body chunks (so arbitrarily large bodies — Claude Code's system prompt
// + tools easily exceed the data channel's per-message limit) and a `reqend`.
func (g *GuestManager) sendRequest(s *guestSession, id, method, path string, headers http.Header, body []byte) error {
	if err := sendFrame(s.dc, frame{
		T: "req", ID: id, Method: method, Path: path,
		Headers: cloneForwardHeaders(headers),
	}); err != nil {
		return err
	}
	const chunk = 16 * 1024
	for off := 0; off < len(body); off += chunk {
		end := off + chunk
		if end > len(body) {
			end = len(body)
		}
		if err := sendFrame(s.dc, frame{T: "reqdata", ID: id, B64: base64.StdEncoding.EncodeToString(body[off:end])}); err != nil {
			return err
		}
	}
	return sendFrame(s.dc, frame{T: "reqend", ID: id})
}

func (g *GuestManager) ensureSession(ctx context.Context, gc *guestConn) (*guestSession, error) {
	gc.mu.Lock()
	s := gc.session
	if s != nil && !s.isClosed() {
		gc.mu.Unlock()
	} else {
		var err error
		s, err = g.newSession(gc)
		if err != nil {
			gc.mu.Unlock()
			return nil, err
		}
		gc.session = s
		gc.mu.Unlock()
	}

	select {
	case <-s.authOK:
		if s.authErr != "" {
			return nil, errors.New(s.authErr)
		}
		return s, nil
	case <-s.closed:
		return nil, errors.New("connection closed before auth")
	case <-time.After(25 * time.Second):
		return nil, errors.New("timed out connecting to friend")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// caller holds gc.mu
func (g *GuestManager) newSession(gc *guestConn) (*guestSession, error) {
	pc, err := webrtc.NewPeerConnection(webrtcConfig())
	if err != nil {
		return nil, err
	}
	dc, err := pc.CreateDataChannel(dataChannelLabel, nil)
	if err != nil {
		pc.Close()
		return nil, err
	}
	s := &guestSession{
		id:      randomID(8),
		pc:      pc,
		dc:      dc,
		keys:    gc.keys,
		authOK:  make(chan struct{}),
		closed:  make(chan struct{}),
		pending: map[string]*pendingReq{},
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cand, _ := json.Marshal(c.ToJSON())
		g.sendSignal(gc, s.id, "ice", cand)
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("guest[%s]: ICE state -> %s", s.id, state)
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("guest[%s]: conn state -> %s", s.id, state)
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			s.close()
		}
	})
	dc.OnOpen(func() {
		log.Printf("guest[%s]: data channel open, sending auth", s.id)
		payload, err := sealJSON(s.keys.channelKey, authPayload{TS: time.Now().Unix()})
		if err == nil {
			_ = sendFrame(dc, frame{T: "auth", Payload: payload})
		}
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.onFrame(msg.Data)
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, err
	}
	offJSON, _ := json.Marshal(offer)
	g.sendSignal(gc, s.id, "offer", offJSON)
	log.Printf("guest[%s]: offer sent to room", s.id)
	return s, nil
}

func (g *GuestManager) sendSignal(gc *guestConn, session, kind string, payload json.RawMessage) {
	content, err := sealJSON(gc.keys.announceKey, signalContent{
		From:    gc.peerID,
		Session: session,
		Kind:    kind,
		Payload: payload,
	})
	if err != nil {
		return
	}
	g.nostr.Publish(gc.keys.roomID, "signal", content)
}

func (s *guestSession) onFrame(data []byte) {
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	switch f.T {
	case "auth_ok":
		s.authOnce.Do(func() { close(s.authOK) })
	case "auth_err":
		s.authErr = f.Msg
		s.authOnce.Do(func() { close(s.authOK) })
	case "head":
		s.deliver(f.ID, respEvent{kind: "head", status: f.Status, ctype: f.Ctype})
	case "data":
		raw, _ := base64.StdEncoding.DecodeString(f.B64)
		s.deliver(f.ID, respEvent{kind: "data", data: raw})
	case "end":
		s.deliver(f.ID, respEvent{kind: "end"})
	case "err":
		s.deliver(f.ID, respEvent{kind: "err", msg: f.Msg})
	}
}

func (s *guestSession) deliver(id string, ev respEvent) {
	s.mu.Lock()
	pr, ok := s.pending[id]
	s.mu.Unlock()
	if !ok {
		return
	}
	// Block until the consumer takes it (back-pressure, never drop a stream
	// frame), but bail the instant the request ends or the session closes so we
	// never wedge pion's read loop.
	select {
	case pr.ch <- ev:
	case <-pr.done:
	case <-s.closed:
	}
}

func (s *guestSession) close() {
	s.closeMu.Do(func() {
		close(s.closed)
		if s.pc != nil {
			_ = s.pc.Close()
		}
	})
}

func (s *guestSession) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// ConnStatus is the guest-side runtime view of one connection.
type ConnStatus struct {
	Online           bool                 `json:"online"`
	Routing          bool                 `json:"routing"`
	Revoked          bool                 `json:"revoked"`
	Paused           bool                 `json:"paused"`                     // host paused sharing (still online)
	PausedReason     string               `json:"pausedReason,omitempty"`     // why, if the host said (e.g. usage limit)
	LimitedProviders []string             `json:"limitedProviders,omitempty"` // providers auto-paused by the host's reserve (partial or full)
	Usage            []providerUsageShare `json:"usage,omitempty"`            // host session windows, as far as they opted to share
	HostName         string               `json:"hostName"`
	Models           []string             `json:"models"`
	TotalReqs        int                  `json:"totalReqs"`
	InputTokens      int64                `json:"inputTokens"`
	OutputTokens     int64                `json:"outputTokens"`
	TokenLimit       int64                `json:"tokenLimit"` // host-advertised allotment (0 = unlimited)
	TokensUsed       int64                `json:"tokensUsed"` // host-authoritative usage, for the remaining calc
}

func (g *GuestManager) Status(connID string) ConnStatus {
	u := g.store.Usage(connID)
	st := ConnStatus{
		TotalReqs:    int(u.Requests),
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
	}

	g.mu.Lock()
	gc, ok := g.conns[connID]
	g.mu.Unlock()
	if !ok {
		return st
	}
	gc.mu.Lock()
	defer gc.mu.Unlock()
	st.Revoked = gc.hostRevoked
	st.Online = !gc.hostRevoked && time.Now().Unix()-gc.lastSeen < 60
	st.Routing = !gc.hostRevoked && gc.active > 0
	st.Paused = gc.hostPaused
	st.PausedReason = gc.hostReason
	st.LimitedProviders = append([]string(nil), gc.hostLimited...)
	if len(gc.hostUsage) > 0 {
		st.Usage = append([]providerUsageShare(nil), gc.hostUsage...)
	}
	st.HostName = gc.hostName
	st.Models = append([]string(nil), gc.hostModels...)
	st.TokenLimit = gc.tokenLimit
	st.TokensUsed = gc.tokensUsed
	return st
}
