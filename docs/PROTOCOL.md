# SWRemote — Wire Protocol

> WebSocket `wss://<relay>/ws`. JSON is UTF-8 text frames; screen/files are binary
> frames. The relay routes everything verbatim — it never parses screen bytes.

## 1. Roles

First message after connect decides the role:

| From → To | Message | Purpose |
|---|---|---|
| agent → relay | `{"t":"register","id":"123456789","name":"DESKTOP-ABC","pinHash":"…"}` | claim an ID |
| viewer → relay | `{"t":"join","id":"123456789","pin":"…"}` | join a device (PIN verified) |

## 2. Relay → agent

| Message | Meaning |
|---|---|
| `{"t":"registered"}` | ID accepted, you are online |
| `{"t":"viewer_joined","viewers":n}` | a viewer connected → agent sends a keyframe |
| `{"t":"viewer_left","viewers":n}` | a viewer disconnected |
| `{"t":"input","k":"move","x":0.5,"y":0.25}` | mouse move (fractions of screen) |
| `{"t":"input","k":"down"/"up","b":"left"/"right"/"middle"}` | mouse button |
| `{"t":"input","k":"scroll","dy":120,"dx":0}` | wheel |
| `{"t":"input","k":"key","key":"a","down":true}` | keyboard |
| `{"t":"input","k":"type","text":"hello"}` | type unicode text |
| `{"t":"quality","q":65,"scale":0.8}` | viewer changed quality/size |
| `{"t":"keyframe"}` | viewer requests a full frame now |
| `{"t":"chat","text":"…"}` | chat message → Windows toast on PC |
| `{"t":"file_meta","id":"…","name":"…","size":n}` | incoming file announced |
| `{"t":"lock"}` | lock the workstation |

## 3. Relay → viewer

| Message | Meaning |
|---|---|
| `{"t":"joined","id","name","screen":{"w","h"},"online":true}` | join accepted |
| `{"t":"error","msg":"wrong pin"}` | join rejected |
| `{"t":"agent_gone"}` / `{"t":"agent_back"}` | device offline/online |
| `{"t":"chat","text":"…"}` | chat from PC side (v3: agent chat window) |
| `{"t":"cursor","x":123,"y":456}` | remote mouse position (px, v3.0+) |
| `{"t":"file_meta",…}` | agent→viewer file (reserved) |

Agent↔viewer JSON is forwarded **verbatim** by the relay, so new `t` types need
no relay changes.

## 4. Binary frames (agent → viewers)

First byte = kind:

| Kind | Layout | Meaning |
|---|---|---|
| `0x01` | `01` + JPEG | **keyframe**: full screen |
| `0x02` | `02` + chunk | file transfer chunk (see below) |
| `0x03` | `03` + uint16BE tileIdx + JPEG | **tile**: one 128×128 region changed |

Tile index: `idx = row * cols + col`, `cols = ceil(width/128)`. The viewer learns
`width/height` from the last keyframe (or the `screen` field in `joined`).

## 5. File transfer

`{"t":"file_meta","id","name","size"}` → then `0x02` + `fileId(16B)` + raw bytes,
repeated until `size` bytes received. Agent writes to `Downloads\SWRemote`.

## 6. HTTP API

| Endpoint | Purpose |
|---|---|
| `GET /api/health` | `{"ok":true}` |
| `GET /api/version` | `{"version":"3.0.0","notes":"…","url":"…"}` |
| `GET /api/devices` | known devices: id, name, online, lastSeen |
| `GET /download/agent` | streams the current `SWRemote-Agent.exe` |
