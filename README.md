# VibeShare

**Share your AI subscriptions with friends — peer-to-peer, no central server.**

VibeShare is a native macOS menu-bar app. Run it and you get a single local
OpenAI-compatible endpoint (`http://127.0.0.1:8788/v1`) that:

1. **Runs a local OpenAI-spec server** your tools can point at.
2. **Connects your LLM providers** (Claude, ChatGPT/Codex, Gemini, Kimi, …) via
   one-click OAuth — powered by the bundled
   [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).
3. **Shares usage with friends** by generating a code they paste in. Sharing is
   one-directional: your code lets them use *your* models; you only get theirs
   if they send you a code.
4. **Shows who's online** and actively routing — discovered over public Nostr
   relays, connected directly over WebRTC (P2P).

When a friend requests a model you don't have, VibeShare auto-routes it to an
online friend who shares it, streaming the response back over a direct,
encrypted WebRTC channel. **No VibeShare server exists** — friends connect
straight to each other.

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the full design.

---

## How it works (30-second version)

```
your tools ──▶ 127.0.0.1:8788/v1  (VibeShare router)
                      │
        ┌─────────────┴──────────────┐
        ▼                            ▼
  local model?               a friend shares it?
  → cli-proxy-api            → WebRTC to their Mac → their cli-proxy-api
    (your subscription)        (their subscription)         │
                                       └──── streamed back ──┘
```

Connection setup (the WebRTC offer/answer) is signaled through public Nostr
relays. Presence ("I'm online, here are my models") is broadcast as **encrypted**
heartbeats only holders of the grant code can read. Once connected, traffic flows
**directly peer-to-peer** over a DTLS-encrypted data channel.

A grant **code** is the only credential. From it both sides derive the same Nostr
room and encryption keys. **Treat codes like passwords.**

## Requirements

- macOS 13 (Ventura) or later
- To build: [Go](https://go.dev) 1.23+ and Swift / Xcode command-line tools

## Build & run

```bash
# 1. Download the provider engine (cli-proxy-api) into Resources
make fetch-provider

# 2a. Quick dev build + run (debug)
make swift
./src/.build/debug/VibeShare      # menu-bar icon appears

# 2b. Build, replace the installed app, and launch it (quit VibeShare first)
make run

# 2c. …or package a distributable disk image (VibeShare-<version>.dmg)
make dmg                          # this machine's arch
make universal-dmg               # Intel + Apple Silicon
```

`make install` copies it to `/Applications`. Without a Developer ID it is
ad-hoc signed (fine locally; right-click → Open the first time).
Quit a running VibeShare before installing, then launch the new app. Replacing
its signed files while the old menu-bar process is still running can leave
Little Snitch matching the old process identity against the new build and block
all relay connections. Check the relay count and a borrowed connection after
relaunching.

`make dmg` builds the app and wraps it in a drag-to-install disk image (app
next to an `Applications` shortcut). With a Developer ID it signs the image;
set `NOTARIZE=1` (plus `NOTARY_PROFILE`, or `APPLE_ID`/`TEAM_ID`/`APP_PASSWORD`)
to also notarize and staple it for distribution to other Macs.

## Using it

1. Click the menu-bar icon → **Providers** → **Connect** your subscriptions
   (opens a browser OAuth flow; credentials are stored locally in
   `~/.cli-proxy-api`). **Disconnect** deletes that stored sign-in again —
   reconnect whenever you like.
2. Point any OpenAI-compatible tool at `http://127.0.0.1:8788/v1` (no API key
   needed locally).

### Share with a friend

- **Share** tab → **New** → name it, pick what to share (everything, specific
  providers, or specific models) and an optional token allotment → **Create
  code**. Send them the `VS-…` code.
- They open VibeShare → **Borrow** → **Enter code**. Within seconds you appear
  **online** in their list, and your shared models show up in their
  `/v1/models`.
- Need your quota back for a while? **Pause** a friend instead of revoking —
  the code stays valid and you can resume any time. **Revoke** kills the code
  permanently and marks it revoked on their Borrow tab when their app sees the
  revoke notice.
- The **Activity** tab shows every request routed through your shares (both
  directions) as it happens.

### Borrow from a friend

- **Borrow** tab → **Enter code** with the code they gave you. Their models now
  resolve through your local endpoint automatically.
- If a friend revokes a code while you are online, VibeShare marks that borrowed
  connection **revoked**, removes it from routing, and leaves the row so you can
  remove it yourself. Hosts also re-announce recent revokes while running, so
  borrowers who come online later can still learn about the revoke.
- Your local model normally wins when you and a friend both have the same
  model. For Claude/Codex models, if VibeShare knows your local 5-hour session
  is exhausted and a non-exhausted friend shares that exact model, it routes to
  the friend instead. If several friends share the same model, requests stick to
  the friend who served it last (keeps their provider-side prompt cache warm),
  prefer whoever has the most allotment left, and automatically fail over to the
  next friend if one is unreachable or out of budget.
- If this Mac only borrows a provider and its local sign-in is unusable, turn on
  **Prefer a friend's models over local models** in Settings (or run
  `vibeshare config set prefer-borrowed on`). A shared model then goes directly
  to the friend even when the local engine still lists it. The default is off.

### Use with Claude Code

Claude Code speaks the Anthropic Messages API (`/v1/messages`), which VibeShare
routes the same way as the OpenAI path — local first unless the local Claude
session is known exhausted, otherwise to an online friend who shares the model.
Point Claude Code at the gateway:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8788 ANTHROPIC_API_KEY=vibeshare claude
```

`ANTHROPIC_API_KEY` only needs to be non-empty (the loopback endpoint doesn't
check it). Tip: wrap it in a shell function so your normal `claude` is unchanged:

```bash
vibeclaude() { ANTHROPIC_BASE_URL=http://127.0.0.1:8788 ANTHROPIC_API_KEY=vibeshare claude "$@"; }
```

## Agents and the CLI

`vibeshare` drives the running menu-bar app over the loopback control API.
Claude Code, Codex, Hermes, and Grok can share, borrow, pause, and change
settings without clicking the menu. `vibeshare help` lists every command.
Add `--json` (or set `VIBESHARE_JSON=1`) when a program is reading the result.

```bash
vibeshare status
vibeshare doctor             # app, router, and provider engine health
vibeshare models             # all models exposed at the local endpoint
vibeshare usage              # every available subscription limit window
vibeshare connections          # friends whose models you are borrowing
vibeshare grants               # codes you have given out
vibeshare grants pause <id>    # stop lending to one friend; borrows stay up
vibeshare env                  # ANTHROPIC_BASE_URL / OPENAI_BASE_URL exports
vibeshare logs router          # last 50 router log lines
vibeshare app restart --yes    # full restart after relay changes
```

Revoking a code, dropping a borrow, and disconnecting a provider require
`--yes`. Provider sign-in (`vibeshare providers login claude`) opens the same
browser flow as the menu and does not run unless you ask for it.
`vibeshare --json` returns the router's JSON for reads and writes. CLI-only
commands return an object (`doctor`, `logs`, `app`) or model list (`models`).
Errors exit nonzero and write `{"ok":false,"error":"..."}` to stderr.
The CLI covers sharing, grants, connections, provider sign-in, usage,
activity, models, logs, and router settings. macOS launch-at-login and native
notification preferences are still managed in the app's Settings tab.

Subscription bars show every limit window returned by a connected provider,
including separate model-family windows such as Claude's weekly Fable limit.
Share cards show the host's windows for the providers in that grant. Borrow
cards show the same windows only when the host opts into the **windows**
usage-sharing level. **Off** shares no subscription details; **resets** shares
only reset times for providers paused by the reserve gate. A friend still
running VibeShare 1.0 will not send these bars until they upgrade. These
percentages describe the host's subscription, separate from the token
allotment assigned to a particular friend.

Version 1.1 speaks the same offer, answer, and data-channel messages as 1.0.
A Mac that has installed 1.1 keeps working with a friend who is still on 1.0.
The newer side is more patient when the path blips and puts ICE candidates in
the SDP, which 1.0 already applies. Install 1.1 on both sides when you can:
both ends then wait out a short disconnect instead of hanging up immediately.

## Ports & state

| Port  | What                                                        |
|-------|-------------------------------------------------------------|
| 8788  | VibeShare OpenAI endpoint (point your tools here)           |
| 8799  | Router control API (used by the app UI; loopback only)      |
| 8317  | cli-proxy-api provider engine (internal; loopback only)     |

State lives in `~/.vibeshare/` (config + grants + connections + logs). Grant
files contain secret codes — never commit or share them.

## Project layout

```
router/     Go engine: OpenAI front server + P2P routing (pion/webrtc + go-nostr)
src/        Swift menu-bar app (manages the two Go binaries, drives the UI)
scripts/    fetch-cliproxyapi.sh
```

## Status

This is an initial, working version (P2P routing of streaming completions is
verified end-to-end). Rough edges / planned next steps:

- Provider connection currently relies on cli-proxy-api's OAuth flows; the
  "connected" indicator is inferred from the live model list.
- WebRTC uses public STUN only — very restrictive NATs may need a TURN server.
- Grant codes are bearer credentials; pausing a grant suspends access (the code
  survives), revoking ends it permanently.
- Relays can be edited in Settings; saving restarts the router automatically
  (relays only apply at router start).

## Credits

Provider authentication and the OpenAI-compatible provider API are handled by
the bundled [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

VibeShare was inspired initially by
[VibeProxy](https://github.com/automazeio/vibeproxy), and its peer-to-peer
signaling draws on ideas from
[SeedShell](https://github.com/Deploydon/seedshell). The code here is its own.

## Disclaimer

VibeShare lets you share access to your LLM provider accounts with other people.
**Doing so may violate the terms of service** of the providers involved (such as
Anthropic, OpenAI, Google, and others) — many subscriptions prohibit sharing
accounts, credentials, or access with third parties.

**You alone are responsible** for ensuring your use of VibeShare complies with
the terms of every provider and account you connect or share. The authors and
contributors **take no responsibility and accept no liability** for any
consequences of using it — including but not limited to account suspension,
termination, lockout, rate-limiting, loss of access, or any other action a
provider may take against you or anyone you share with.

VibeShare is provided "as is", without warranty of any kind. **Use it at your
own risk.**

## License

MIT 
