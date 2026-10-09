# SWRemote — Architecture

> In-depth technical record of the SWRemote system. Last updated: 2026-10-09 (v3.0).

## 1. What SWRemote is

SWRemote is an AnyDesk-style remote desktop product by SWInfoSystems with three parts:

| Part | Tech | Runs on |
|---|---|---|
| **Agent** | Go, pure Win32 syscalls, no cgo | Windows PC being controlled |
| **Relay** | Node.js + `ws` WebSocket server | Render.com (free tier) |
| **Console** | Vanilla JS + Canvas, PWA | Any browser / phone |

Design rule: the PC side must be AnyDesk-simple — open the exe, get an ID + PIN, done.
No config files to edit, no admin rights needed.

## 2. How the pieces connect

```
 ┌────────────┐   WSS (443)    ┌──────────────┐   WSS (443)    ┌────────────┐
 │  AGENT     │◄──────────────►│    RELAY     │◄──────────────►│  CONSOLE   │
 │ (Windows)  │  register/ping │ (Render)     │  join/input    │ (browser)  │
 └────────────┘  screen tiles  └──────────────┘  screen tiles └────────────┘
       │                              │
  screen capture                  static files
  input injection                 /api/*, /download/agent
```

- The agent opens **one outbound WebSocket** to the relay (`wss://…/ws`, role=`agent`).
  Outbound-only means it works behind NAT/firewalls with zero router config.
- The console (viewer) opens a WebSocket with role=`viewer` and joins a device by ID + PIN.
- The relay **never interprets screen data** — it routes opaque JSON and binary packets
  between the agent and its viewers. See `docs/PROTOCOL.md`.

## 3. Agent internals (Go)

```
main.go      — entry, config, network loops, streaming engine, input, files, updater
gui.go       — native Win32 window (ID/PIN/status/viewers/uptime/buttons)
update.go    — version check + self-update via updater batch script
log.go       — %LocalAppData%\SWRemote\agent.log + panic-safe goroutines
```

Key subsystems:

- **Config** (`%LocalAppData%\SWRemote\config.json`): device ID (9 digits), PIN (6 digits),
  FPS, JPEG quality, scale, server URL. Auto-generated on first run.
- **Streaming engine** (`main.go`, "tiled streaming" section): captures the screen,
  splits it into 128×128 tiles, diffs each tile against the previous frame and sends
  only changed tiles as JPEG. Full keyframe every 4 s, on viewer join, or on request.
  See `docs/PERFORMANCE.md` for why this replaced full-frame streaming in v3.0.
- **Input injection**: mouse (absolute move, buttons, wheel) via `SendInput`,
  keyboard via `SendInput` with scancodes + Unicode fallback for text typing.
- **File receive**: viewer sends `file_meta` + binary chunks (`0x02`); agent writes to
  `Downloads\SWRemote`.
- **Chat**: viewer→agent text shows a Windows toast (`notify`); agent has no chat
  window yet (v3 roadmap).
- **Self-update**: checks `/api/version`; downloads new exe to temp; writes an
  updater `.bat` that waits for exit, swaps the exe, restarts it. Update flow is
  guarded against re-entrancy (atomic flag).
- **Reliability**: every network goroutine wrapped in `safeGo` (recovers panics to the
  log); 60 s handshake timeout for Render cold starts; HTTPS keep-alive every
  10 min so the free relay doesn't sleep-disconnect the agent; detailed disconnect
  reasons in `agent.log`.

## 4. Relay internals (Node)

`server/server.js` — one file, ~230 lines:

- Serves the console static files (`/`), PWA assets, `/api/health`, `/api/version`,
  `/api/devices` (online list + last-seen), `/download/agent` (streams the exe).
- WebSocket roles: `agent` (registers with ID + PIN hash) and `viewer` (joins with
  ID + PIN). PINs are verified by hash comparison — the relay never stores the PIN.
- Routes JSON both ways verbatim; routes binary packets agent→viewers.
- Tracks viewer counts per device, emits `viewer_joined` / `viewer_left`.
- `devices.json` persists the known-device list (name, last-seen) across restarts.

## 5. Console internals (JS)

`console/` — no framework, no build step:

- `index.html` — three screens (server/login, devices, session) + PIN modal + sheets.
- `app.js` — device list, session lifecycle, canvas input (mouse/touch/keyboard),
  pinch-zoom (1–4×), tile compositor, chat + file sheets, quality popover, PWA install.
- `style.css` — calm-light design system (Plus Jakarta Sans).
- `sw.js` — service worker, offline-first app shell.

## 6. Security model (current)

- Every session needs the 6-digit PIN (compared as salted hash on the relay).
- No account system yet — anyone who knows a device ID can *try* its PIN.
  Device IDs are 9 random digits (not enumerable in practice).
- v3 roadmap replaces this with Supabase accounts + private device lists;
  ID+PIN remains as the "quick help" path. See `docs/ROADMAP-V3.md`.
