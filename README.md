# SWRemote v1 — by SWInfoSystems

Your own AnyDesk-style remote desktop: a **Windows app** that shares its screen and
takes remote control, a **relay server** that connects devices and viewers, and a
**website** that manages everything and installs as an app on phones.

```
Windows PC (agent)  ──WebSocket──▶  Relay server (Node.js)  ◀──WebSocket──  Phone / browser (console PWA)
   screen frames ──────────────────────▶││▶────────────────────── live canvas
   ◀────────────────────── mouse / keyboard / files / chat
```

## What's inside

| Folder | What it is |
|---|---|
| `agent-go/` | **Windows app (native `.exe`, recommended)** — pure Go, cross-compiled. Screen share, remote mouse/keyboard via WinAPI SendInput, file receive, chat, remote lock. No Python needed. |
| `agent/` | Windows app (Python alternative): same features via `install.bat`, needs Python on the PC. |
| `server/` | Relay server (Node.js + WebSocket). Device registry in a free local JSON file — no database needed. Also serves the console website. |
| `console/` | Management website (installable PWA): device list, PIN login, live viewer, touch controls, chat, file send, quality settings. |

## Setup — 3 steps

### 1. Put the relay server online (free options)

The server needs a public HTTPS address so your phone and PCs can reach it.

**Option A — Render (free, easiest).** New → Web Service → upload the `server/`
folder (or push to GitHub) → Build command `npm install`, Start command
`npm start`. Render gives you `https://your-name.onrender.com` with TLS built in.
Note: the free tier sleeps after inactivity; first connection takes ~30s to wake.

**Option B — Railway / Fly.io (free tiers).** Same two commands; both give TLS.

**Option C — your own PC (one double-click, no card, no account).** Copy the
whole `server/` folder to the Windows PC and run `run-relay-tunnel.bat` — it
installs the relay, starts it, and opens a free Cloudflare quick tunnel, printing
a public `https://xxxx.trycloudflare.com` address. The relay AND the phone console
both run from that one address, so this option needs neither Render nor Vercel.
Tradeoff: the address changes each time the file is re-run, and the PC must stay
on and connected while you want remote access. (Manual alternative: `cd server &&
npm install && npm start`, then expose port 8080 yourself.)

### 2. Install the agent on each Windows PC

**Recommended — the ready `.exe` (no Python needed):**
1. Copy `agent-go/SWRemote-Agent.exe` to the Windows PC.
2. Double-click it once — it creates `swremote.json` next to itself and prints your
   **9-digit SWRemote ID** and PIN.
3. Edit `swremote.json` (Notepad): set `server` to your relay address
   (e.g. `wss://swremote-relay.onrender.com/ws` — note `wss`, not `https`),
   and change `pin` to your own secret PIN.
4. Run the `.exe` again and keep it open while you want the PC reachable.
   Tip: press Win+R, type `shell:startup`, and drop a shortcut there for auto-start.

**Alternative — Python agent** (`agent/` folder): double-click `install.bat`
(installs Python libraries once, needs internet), enter server + PIN when asked.
Same features; needs Python on the PC.

### 3. Open the console on your phone

1. Go to `https://your-name.onrender.com` (your server address) in the phone browser.
2. Enter the server address once → your devices appear with online dots.
3. Tap **Connect**, enter the device **PIN** → live screen.
4. **Add to Home Screen** — it installs as a standalone app (PWA).
5. Controls: tap = click, drag = move mouse, **two-finger tap = right-click**,
   ⌨ button = keyboard (incl. Esc/Win/arrows), 💬 chat, 📁 send files to the PC,
   🔒 lock the PC remotely, ⚙ quality/speed.

## Security notes (read this)

- Every device is protected by its own PIN (stored on the server as a hash, never plain).
- Always use `https://` / `wss://` server addresses (Render/Railway/Fly give this free) —
  traffic is TLS-encrypted in transit.
- This is a **v1 prototype**: no end-to-end encryption, no brute-force lockout yet —
  use long PINs and don't expose the server URL publicly.

## Honest limits of v1

- Screen is JPEG frames at ~5–10 fps (not 60 fps like AnyDesk) — fine for office work, not gaming/video.
- The Windows agent needs Python on the PC (or a PyInstaller `.exe` you build on Windows).
- No unattended UAC prompts (Windows blocks input on secure desktop) — same limit most remote tools have.
- Server registry is a local JSON file: fine for tens of devices; move to a real DB later if you grow.

## Tested

`server/test-e2e.js` — 11/11 passing against the real server: registration, PIN
auth (right + wrong), video frames, input relay, chat, file transfer, offline
detection, device-list privacy (no PINs leak). The console UI was visually verified
in headless Chromium: login, device list, and a live session rendering frames.
