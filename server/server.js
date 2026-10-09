/*
 * SWRemote relay server v1
 * Pairs Windows agents with remote viewers over a single WebSocket endpoint.
 * Storage: free local JSON file (devices.json). No database needed.
 *
 * Run:  npm install && npm start        (env: PORT, SWREMOTE_ADMIN)
 */
"use strict";

const http = require("http");
const fs = require("fs");
const path = require("path");
const crypto = require("crypto");
const { WebSocketServer } = require("ws");

const PORT = parseInt(process.env.PORT || "8080", 10);
const ADMIN_KEY = process.env.SWREMOTE_ADMIN || ""; // optional key guarding full device info
const DATA_FILE = path.join(__dirname, "devices.json");
const CONSOLE_DIR = path.join(__dirname, "..", "console");

// ---------- tiny JSON store (free local storage) ----------
let store = { devices: {} };
try {
  if (fs.existsSync(DATA_FILE)) store = JSON.parse(fs.readFileSync(DATA_FILE, "utf8"));
} catch (e) { console.error("Could not read devices.json, starting fresh:", e.message); }

function saveStore() {
  try { fs.writeFileSync(DATA_FILE, JSON.stringify(store, null, 2)); }
  catch (e) { console.error("Could not write devices.json:", e.message); }
}

function pinHash(deviceId, pin) {
  return crypto.createHash("sha256").update(deviceId + "::" + pin).digest("hex");
}

// ---------- live state ----------
const agents = new Map();   // deviceId -> { ws, name, platform, screen, lastSeen, viewers:Set }
const viewers = new Map();  // ws -> { deviceId }

function send(ws, obj) {
  if (ws.readyState === 1) ws.send(JSON.stringify(obj));
}

function devicePublic(id, d, live) {
  return {
    id,
    name: d.name || id,
    platform: d.platform || "windows",
    online: !!(live && live.ws && live.ws.readyState === 1),
    viewers: live ? live.viewers.size : 0,
    screen: live ? live.screen : null,
    lastSeen: live ? live.lastSeen : (d.lastSeen || null),
  };
}

// ---------- HTTP: health + device list + static console ----------
const MIME = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".json": "application/json", ".png": "image/png", ".svg": "image/svg+xml", ".ico": "image/x-icon", ".webmanifest": "application/manifest+json" };

const httpServer = http.createServer((req, res) => {
  const url = new URL(req.url, "http://x");
  // CORS: the console may be hosted on another origin (e.g. Vercel) than this relay
  const cors = { "Access-Control-Allow-Origin": "*", "Access-Control-Allow-Methods": "GET,OPTIONS", "Access-Control-Allow-Headers": "Content-Type" };
  if (req.method === "OPTIONS") { res.writeHead(204, cors); return res.end(); }
  if (url.pathname === "/api/health") {
    res.writeHead(200, { "Content-Type": "application/json", ...cors });
    return res.end(JSON.stringify({ ok: true, service: "swremote-relay", ts: Date.now(), online: agents.size }));
  }
  if (url.pathname === "/api/devices") {
    const full = ADMIN_KEY && url.searchParams.get("admin") === ADMIN_KEY;
    const list = Object.entries(store.devices).map(([id, d]) => {
      const p = devicePublic(id, d, agents.get(id));
      return full ? p : { id: p.id, name: p.name, platform: p.platform, online: p.online, lastSeen: p.lastSeen };
    });
    res.writeHead(200, { "Content-Type": "application/json", ...cors });
    return res.end(JSON.stringify({ devices: list }));
  }
  // static console
  let p = url.pathname === "/" ? "/index.html" : url.pathname;
  const file = path.normalize(path.join(CONSOLE_DIR, p));
  if (!file.startsWith(CONSOLE_DIR) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) {
    res.writeHead(404); return res.end("not found");
  }
  res.writeHead(200, { "Content-Type": MIME[path.extname(file)] || "application/octet-stream" });
  fs.createReadStream(file).pipe(res);
});

// ---------- WebSocket ----------
const wss = new WebSocketServer({ server: httpServer, path: "/ws" });

wss.on("connection", (ws) => {
  let role = null; // "agent" | "viewer"
  let deviceId = null;

  ws.on("message", (raw, isBinary) => {
    // ---- binary: 1-byte kind prefix; 0x01 video frame, 0x02 file chunk ----
    if (isBinary) {
      const kind = raw[0];
      if (role === "agent" && deviceId) {
        const rec = agents.get(deviceId);
        if (rec) for (const v of rec.viewers) if (v.readyState === 1) v.send(raw);
      } else if (role === "viewer" && deviceId) {
        const rec = agents.get(deviceId);
        if (rec && rec.ws.readyState === 1) rec.ws.send(raw);
      }
      return;
    }

    let msg;
    try { msg = JSON.parse(raw.toString()); } catch { return send(ws, { t: "error", msg: "bad json" }); }

    // ---- agent registers ----
    if (msg.t === "register") {
      if (!msg.id || !msg.pin) return send(ws, { t: "error", msg: "id and pin required" });
      deviceId = String(msg.id);
      role = "agent";
      const prev = agents.get(deviceId);
      if (prev && prev.ws !== ws) { try { prev.ws.close(4000, "replaced"); } catch {} }
      const rec = {
        ws,
        name: msg.name || deviceId,
        platform: msg.platform || "windows",
        screen: msg.screen || null,
        lastSeen: Date.now(),
        viewers: prev ? prev.viewers : new Set(),
      };
      agents.set(deviceId, rec);
      // persist device (pin stored as salted hash only)
      store.devices[deviceId] = {
        name: rec.name, platform: rec.platform,
        pinHash: pinHash(deviceId, String(msg.pin)),
        lastSeen: rec.lastSeen,
      };
      saveStore();
      // re-attach existing viewers to the new socket
      for (const v of rec.viewers) { viewers.set(v, { deviceId }); send(v, { t: "agent_back", screen: rec.screen }); }
      return send(ws, { t: "registered", id: deviceId, viewers: rec.viewers.size });
    }

    if (msg.t === "ping" && role === "agent") {
      const rec = agents.get(deviceId);
      if (rec) rec.lastSeen = Date.now();
      return send(ws, { t: "pong" });
    }

    // ---- viewer joins a device ----
    if (msg.t === "join") {
      const id = String(msg.id || "");
      const d = store.devices[id];
      if (!d) return send(ws, { t: "error", msg: "unknown device" });
      if (pinHash(id, String(msg.pin || "")) !== d.pinHash)
        return send(ws, { t: "error", msg: "wrong pin" });
      role = "viewer"; deviceId = id;
      viewers.set(ws, { deviceId: id });
      const rec = agents.get(id);
      if (rec && rec.ws.readyState === 1) {
        rec.viewers.add(ws);
        send(ws, { t: "joined", id, name: d.name, screen: rec.screen, online: true });
        send(rec.ws, { t: "viewer_joined", viewers: rec.viewers.size });
      } else {
        send(ws, { t: "joined", id, name: d.name, screen: null, online: false });
      }
      return;
    }

    // ---- relay JSON both ways ----
    if (role === "viewer" && deviceId) {
      const rec = agents.get(deviceId);
      if (rec && rec.ws.readyState === 1) rec.ws.send(JSON.stringify(msg));
      else send(ws, { t: "error", msg: "device offline" });
      return;
    }
    if (role === "agent" && deviceId) {
      const rec = agents.get(deviceId);
      if (rec) for (const v of rec.viewers) if (v.readyState === 1) v.send(JSON.stringify(msg));
      return;
    }
  });

  ws.on("close", () => {
    if (role === "agent" && deviceId) {
      const rec = agents.get(deviceId);
      if (rec && rec.ws === ws) {
        agents.delete(deviceId);
        for (const v of rec.viewers) { viewers.delete(v); send(v, { t: "agent_gone" }); }
        if (store.devices[deviceId]) { store.devices[deviceId].lastSeen = Date.now(); saveStore(); }
      }
    }
    if (role === "viewer" && deviceId) {
      viewers.delete(ws);
      const rec = agents.get(deviceId);
      if (rec) { rec.viewers.delete(ws); send(rec.ws, { t: "viewer_left", viewers: rec.viewers.size }); }
    }
  });
});

httpServer.listen(PORT, () => {
  console.log(`SWRemote relay listening on :${PORT}  (ws path /ws, console at /)`);
});
