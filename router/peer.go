package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/pion/webrtc/v3"
)

// webrtcConfig returns the ICE configuration used for all peer connections.
// Public STUN is enough for most NATs; a TURN server can be added later for
// the restrictive ones.
func webrtcConfig() webrtc.Configuration {
	return webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
	}
}

// dataChannelLabel is the single reliable, ordered channel used per session.
const dataChannelLabel = "vibeshare"

// signalContent is the decrypted body of a `t:"signal"` Nostr event. It is
// sealed with the grant's announceKey before it ever touches a relay.
type signalContent struct {
	From    string          `json:"from"`    // ephemeral origin id (guest session)
	Session string          `json:"session"` // session id, lets a host serve many guests in one room
	Kind    string          `json:"kind"`    // "offer" | "answer" | "ice"
	Payload json.RawMessage `json:"payload"` // SDP or ICE candidate JSON
}

// presenceContent is the decrypted body of a `t:"presence"` Nostr event.
type presenceContent struct {
	Role             string   `json:"role"`             // "host" | "guest"
	Name             string   `json:"name,omitempty"`   // host's identity label
	Models           []string `json:"models,omitempty"` // models the host is sharing via this grant
	Peer             string   `json:"peer,omitempty"`   // guest's ephemeral session id
	Routing          bool     `json:"routing,omitempty"`
	Revoked          bool     `json:"revoked,omitempty"` // host→guest: grant was revoked permanently
	Paused           bool     `json:"paused,omitempty"`  // host→guest: sharing temporarily paused (still online)
	Reason           string   `json:"reason,omitempty"`  // host→guest: why fully paused (e.g. session usage limit)
	LimitedProviders []string `json:"limited,omitempty"` // host→guest: providers auto-paused by the reserve (partial or full)
	// Usage carries the host's own session-window state for the providers behind
	// this grant, so the guest can say *when* a pause lifts — and, at the most
	// open setting, see the squeeze coming before it costs them a request. Sent
	// only as far as Config.ShareUsageLevel allows: empty at "off", reset times
	// for already-paused providers at "resets", full windows at "windows".
	Usage      []providerUsageShare `json:"usage,omitempty"`
	TokenLimit int64                `json:"tokenLimit,omitempty"` // host→guest: this friend's allotment (0 = unlimited)
	TokensUsed int64                `json:"tokensUsed,omitempty"` // host→guest: authoritative tokens consumed so far
	TS         int64                `json:"ts"`
}

// providerUsageShare is one provider's session window as disclosed to a guest.
// Utilization is omitted (not zero) at the "resets" level, so a guest can tell
// "0% used" from "the host didn't share a number" and render accordingly.
type providerUsageShare struct {
	Provider    string   `json:"provider"`              // display name, e.g. "Codex"
	Limited     bool     `json:"limited,omitempty"`     // reserve gate has paused this provider
	Utilization *float64 `json:"utilization,omitempty"` // percent of the session window used, 0..100
	ResetsAt    string   `json:"resetsAt,omitempty"`    // RFC3339, "" when unknown
}

// frame is a JSON message sent over the WebRTC DataChannel.
type frame struct {
	T       string      `json:"t"`                 // auth|auth_ok|auth_err|req|head|data|end|err
	ID      string      `json:"id,omitempty"`      // request id (req/head/data/end/err)
	Method  string      `json:"method,omitempty"`  // request method (req); defaults to POST
	Path    string      `json:"path,omitempty"`    // request path (req)
	Headers http.Header `json:"headers,omitempty"` // safe client headers (req)
	Status  int         `json:"status,omitempty"`  // response status (head)
	Ctype   string      `json:"ctype,omitempty"`   // response content-type (head)
	B64     string      `json:"b64,omitempty"`     // response body chunk, base64 (data)
	Msg     string      `json:"msg,omitempty"`     // error / auth message
	Payload string      `json:"payload,omitempty"` // sealed {ts} for auth
}

func sendFrame(dc *webrtc.DataChannel, f frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return dc.SendText(string(b))
}

// randomID returns n random bytes hex-encoded (2n chars).
func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// authPayload is the plaintext sealed inside an `auth` frame's payload.
type authPayload struct {
	TS int64 `json:"ts"`
}
