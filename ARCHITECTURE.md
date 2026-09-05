# VibeShare — Architecture

VibeShare is a native macOS menu-bar app that lets you **share your LLM
subscriptions with friends, peer-to-peer, with no central server.**

```
┌──────────────────────────────────────────────────────────────────────────┐
│  VibeShare.app  (Swift / SwiftUI menu-bar — thin manager UI)               │
│                                                                            │
│   supervises two bundled Go binaries as child processes:                   │
│                                                                            │
│   ┌────────────────────────┐         ┌──────────────────────────────────┐ │
│   │  cli-proxy-api          │  HTTP   │  vibeshare-router                │ │
│   │  (provider engine)      │◄────────┤                                  │ │
│   │  127.0.0.1:8317         │ local   │  Front server   127.0.0.1:8788   │ │ ← point your tools here
│   │  • Claude/Codex/Gemini  │ forward │  Control API    127.0.0.1:8799   │ │ ← Swift UI talks here
│   │    OAuth + API keys     │         │  P2P (pion + go-nostr)           │ │
│   │  • multi-account        │         │                                  │ │
│   │  • OpenAI + Anthropic    │         └───────────────┬──────────────────┘ │
│   └────────────────────────┘                         │                    │
└──────────────────────────────────────────────────────┼────────────────────┘
                                                        │ WebRTC DataChannel
                              Nostr public relays       │ (DTLS encrypted)
                              (signaling + presence)     ▼
                                              ┌────────────────────────────┐
                                              │  Friend's VibeShare router  │
                                              │  → their cli-proxy-api      │
                                              └────────────────────────────┘
```

A thin SwiftUI app supervises two bundled Go binaries:

* **`cli-proxy-api`** (`127.0.0.1:8317`) — the provider engine: per-provider
  OAuth/API-key auth, token refresh, multi-account round-robin, exposed as an
  OpenAI- and Anthropic-compatible API. Bundled unchanged.
* **`vibeshare-router`** — the OpenAI/Anthropic **front endpoint** (`:8788`,
  what your tools use), a loopback **control API** (`:8799`, what the UI drives),
  and the **P2P layer**.

The control API (JSON over loopback) is the only seam between Swift and Go, so
the two halves evolve independently. The P2P layer is pure Go (pion/webrtc +
go-nostr), so it ships as an ordinary binary — no WebRTC system framework.

## One-directional "grant" model

Sharing is **one-directional**:

* A **host** creates a **grant** — a random code — choosing which providers/models
  it exposes, and hands the code to a friend.
* The **guest** redeems the code and can route requests for *only* the shared
  models through the host.
* Reverse access requires the friend to issue their own grant.

The grant code is the **only** credential — the Nostr room, the presence/signaling
key, and the data-channel auth key are all derived from it, so treat codes like
passwords.

### Code → keys (`crypto.go`)

```
codeBytes   = base32-decode(code)                 # 16 random bytes
roomID      = base64url( HKDF-SHA256(codeBytes, "vibeshare-grant-room-v1",     16) )
announceKey =            HKDF-SHA256(codeBytes, "vibeshare-grant-announce-v1", 32)   # secretbox: presence + signaling
channelKey  =            HKDF-SHA256(codeBytes, "vibeshare-grant-channel-v1",  32)   # secretbox: data-channel auth
```

Host and guest derive these identically. `roomID` is the Nostr `#d` topic both
sides use; presence and SDP/ICE signaling are sealed with `announceKey`, so relays
only ever see ciphertext.

## P2P protocol

Signaling and presence ride on public Nostr relays as kind **25050** events,
tagged `["d", roomID]` and a `t` type. All content is sealed with `announceKey`.

| `t`        | direction    | content (decrypted JSON)                                                    |
|------------|--------------|-----------------------------------------------------------------------------|
| `presence` | host → room  | `{role:"host", name, models:[...], paused?, revoked?, limited?, usage?, tokenLimit?, tokensUsed?, ts}` |
| `presence` | guest → room | `{role:"guest", peer, routing, ts}`                                         |
| `signal`   | both         | `{from, session, kind:"offer"\|"answer"\|"ice", payload}`                   |

Once offer/answer/ICE complete, a direct WebRTC **DataChannel** (reliable,
ordered, DTLS-encrypted) carries the traffic — Nostr is no longer used. Frames
are JSON text messages; request and response bodies are base64-chunked to stay
under the channel's per-message size limit:

| frame                | direction    | fields                                          |
|----------------------|--------------|-------------------------------------------------|
| `auth`               | guest → host | `secretbox(channelKey, {ts})` — proves the code |
| `auth_ok` / `auth_err` | host → guest | `{msg?}`                                       |
| `req`                | guest → host | `{id, method, path}` — start request            |
| `reqdata`            | guest → host | `{id, b64}` — request-body chunk                |
| `reqend`             | guest → host | `{id}` — end of request body                    |
| `head`               | host → guest | `{id, status, ctype}`                           |
| `data`               | host → guest | `{id, b64}` — response-body chunk               |
| `end` / `err`        | host → guest | `{id, msg?}`                                    |

The **host enforces** that `path` is allow-listed (`/v1/chat/completions`,
`/v1/completions`, `/v1/embeddings`, `/v1/messages`) and that the request's
`model` is in the grant's shared set, then reverse-proxies to its local
`cli-proxy-api` and streams the response back. Streaming (SSE) is transparent.

## Front-server routing (`server.go`)

* `GET /v1/models` → union of **local** models (from `cli-proxy-api`) and
  **remote** models advertised by online friends; local wins on collision.
* `POST /v1/chat/completions` (OpenAI) and `POST /v1/messages` (Anthropic /
  Claude Code) → serve locally if the model is available, unless it is a
  monitored Claude/Codex model and the local 5-hour session is known exhausted.
  In that exhaustion case, or when the model is not local, **auto-route** to an
  online friend that shares it, else `404`. Both carry a top-level `model`, so
  the routing logic is shared.

When several friends share the same model, the guest ranks them (`guest.go
hostsForModel`): **sticky** first (the host that last served this model — keeps
the provider-side prompt cache warm), then most remaining allotment (unlimited
outranks finite; presence carries `tokenLimit`/`tokensUsed`), with exhausted
allotments last. If an attempt fails before any response bytes are written
(host offline, refused, allotment exhausted), the request **fails over** to the
next-ranked friend, up to 3 attempts; each attempt is logged to the activity
feed.

## Control API (`control.go`) — Swift ⇆ router, `127.0.0.1:8799`

| Method & path                  | purpose                                                  |
|--------------------------------|----------------------------------------------------------|
| `GET /api/status`              | router/upstream/Nostr health, identity, local models     |
| `GET /api/grants`              | grants I issued + each guest's online/routing/usage state |
| `POST /api/grants`             | create a grant `{label, providers, models}` → `code`     |
| `PATCH /api/grants/{id}`       | partial update: `{tokenLimit?}` top-up and/or `{paused?}` |
| `DELETE /api/grants/{id}`      | revoke a grant                                           |
| `GET /api/connections`         | grants I hold + host online/models/routing/usage state   |
| `POST /api/connections`        | redeem a code `{code, label}`                            |
| `DELETE /api/connections/{id}` | drop a held connection                                  |
| `GET /api/config` / `PUT`      | read/update identity name, ports, relays                 |
| `GET /api/events`              | SSE stream of state changes (live UI updates)            |
| `GET /api/activity`            | recent routed requests, both directions (live feed)      |

A grant can be **paused**: the host keeps broadcasting presence (with
`paused:true` and no models) but refuses requests, so the guest sees "paused"
rather than "offline" and the code survives to be resumed — unlike revoke,
which kills the code permanently.

The reserve gate can auto-pause a provider whose 5-hour session window passes
the host's threshold; presence then names it in `limited`. How much context rides
along is the host's choice (`shareUsageLevel`): nothing, the paused provider's
reset time, or the live session percentage for the providers behind that grant.
Disclosure never exceeds the grant's own providers, so a Claude-only grant learns
nothing about the host's Codex quota.

When a grant is **revoked**, the host publishes a terminal presence with
`revoked:true` before leaving the room. Guests persist that state on the held
connection, hide its models from `/v1/models`, exclude it from routing, and show
the row as removable in the Borrow tab. The host also republishes recent revoke
tombstones while running, so a guest that was offline at the exact revoke moment
can still observe the terminal state later.

## On-disk state (`~/.vibeshare/`)

* `config.json` — ports, upstream URL + key, Nostr relays, identity name
* `grants.json` — grants I issued (includes the secret code, so rooms survive restarts)
* `connections.json` — grants I hold (includes the redeemed code)
* `usage.json` — persisted per-grant/connection request and token counters

`cli-proxy-api` keeps its own provider credentials in `~/.cli-proxy-api/`.

## Repository layout

```
VibeShare/
├── ARCHITECTURE.md  README.md  Makefile  create-app-bundle.sh  entitlements.plist
├── scripts/fetch-cliproxyapi.sh
├── router/                       ← Go module (vibeshare-router)
│   ├── main.go config.go crypto.go nostr.go peer.go usage.go
│   └── host.go guest.go server.go upstream.go control.go
└── src/                          ← Swift package (menu-bar app)
    ├── Package.swift  Info.plist
    └── Sources/
        ├── App.swift  AppDelegate.swift  AppController.swift  AppPaths.swift
        ├── RouterClient.swift  ProcessManager.swift  ProviderManager.swift  Models.swift
        ├── MenuContentView.swift
        └── Resources/  (config.yaml + bundled cli-proxy-api-plus & vibeshare-router)
```
