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

// ---------- Supabase (Phase 1: accounts) ----------
// Set SUPABASE_URL + SUPABASE_ANON_KEY in the environment to enable accounts.
// Without them the relay behaves exactly as before (ID+PIN only).
const SUPABASE_URL = (process.env.SUPABASE_URL || "").replace(/\/+$/, "");
const SUPABASE_ANON_KEY = process.env.SUPABASE_ANON_KEY || "";
const sbOn = !!(SUPABASE_URL && SUPABASE_ANON_KEY);

async function sb(path, { method = "GET", body = null, token = null, upsert = false } = {}) {
  const r = await fetch(SUPABASE_URL + path, {
    method,
    headers: {
      apikey: SUPABASE_ANON_KEY,
      Authorization: "Bearer " + (token || SUPABASE_ANON_KEY),
      "Content-Type": "application/json",
      Prefer: upsert ? "return=representation,resolution=merge-duplicates" : "return=representation",
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const txt = await r.text();
  let json = null;
  try { json = txt ? JSON.parse(txt) : null; } catch {}
  return { ok: r.ok, status: r.status, json };
}

// verify a Supabase access token -> { id, email } or null
async function sbVerify(token) {
  if (!sbOn || !token) return null;
  try {
    const r = await sb("/auth/v1/user", { token });
    if (r.ok && r.json && r.json.id) return { id: r.json.id, email: r.json.email };
  } catch {}
  return null;
}

// does this user own the device with this agent_id?
async function sbOwns(token, agentId) {
  try {
    const r = await sb("/rest/v1/devices?agent_id=eq." + encodeURIComponent(agentId) + "&select=id", { token });
    return r.ok && Array.isArray(r.json) && r.json.length > 0;
  } catch { return false; }
}

// list the user's claimed devices from Supabase
async function sbMyDevices(token) {
  try {
    const r = await sb("/rest/v1/devices?select=id,agent_id,name,last_seen&order=name", { token });
    if (r.ok && Array.isArray(r.json)) return r.json;
  } catch {}
  return [];
}

function bearerToken(req) {
  const h = req.headers.authorization || "";
  const m = h.match(/^Bearer\s+(.+)$/i);
  return m ? m[1] : null;
}

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
    // Phase 1: with Supabase on, devices are private per account.
    if (sbOn) {
      const finish = (list) => {
        res.writeHead(200, { "Content-Type": "application/json", ...cors });
        res.end(JSON.stringify({ devices: list }));
      };
      (async () => {
        const user = await sbVerify(bearerToken(req));
        if (!user) return finish([]);
        const rows = await sbMyDevices(bearerToken(req));
        finish(rows.map((row) => {
          const live = agents.get(row.agent_id);
          return {
            id: row.agent_id, name: row.name || row.agent_id,
            platform: "windows",
            online: !!(live && live.ws && live.ws.readyState === 1),
            viewers: live ? live.viewers.size : 0,
            screen: live ? live.screen : null,
            lastSeen: live ? live.lastSeen : (row.last_seen || null),
            mine: true,
          };
        }));
      })().catch(() => finish([]));
      return;
    }
    const full = ADMIN_KEY && url.searchParams.get("admin") === ADMIN_KEY;
    const list = Object.entries(store.devices).map(([id, d]) => {
      const p = devicePublic(id, d, agents.get(id));
      return full ? p : { id: p.id, name: p.name, platform: p.platform, online: p.online, lastSeen: p.lastSeen };
    });
    res.writeHead(200, { "Content-Type": "application/json", ...cors });
    return res.end(JSON.stringify({ devices: list }));
  }
  if (url.pathname === "/api/config") {
    // public client config: Supabase URL + anon key (public by design)
    res.writeHead(200, { "Content-Type": "application/json", ...cors });
    return res.end(JSON.stringify({
      supabaseUrl: SUPABASE_URL || null,
      supabaseAnonKey: SUPABASE_ANON_KEY || null,
    }));
  }
  if (url.pathname === "/api/claim" && req.method === "POST") {
    // claim a device by its 6-char code shown in the agent window
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", async () => {
      const done = (code, obj) => {
        res.writeHead(code, { "Content-Type": "application/json", ...cors });
        res.end(JSON.stringify(obj));
      };
      if (!sbOn) return done(503, { error: "accounts not enabled" });
      const user = await sbVerify(bearerToken(req));
      if (!user) return done(401, { error: "sign in required" });
      let code = "";
      try { code = String(JSON.parse(body || "{}").claim_code || "").trim().toUpperCase(); } catch {}
      if (!code) return done(400, { error: "claim_code required" });
      const entry = Object.entries(store.devices).find(([, d]) => (d.claimCode || "").toUpperCase() === code);
      if (!entry) return done(404, { error: "code not found or already claimed" });
      const [agentId, d] = entry;
      try {
        // ensure the profiles row exists (self-heals if the DB trigger is missing)
        await sb("/rest/v1/profiles", { method: "POST", token: bearerToken(req), upsert: true, body: [{ id: user.id }] });
        const r = await sb("/rest/v1/devices", {
          method: "POST",
          token: bearerToken(req),
          body: [{ user_id: user.id, agent_id: agentId, name: d.name || agentId, claimed_at: new Date().toISOString() }],
        });
        if (!r.ok) return done(502, { error: "could not save claim" });
      } catch { return done(502, { error: "could not save claim" }); }
      delete d.claimCode;
      saveStore();
      done(200, { ok: true, id: agentId, name: d.name || agentId });
    });
    return;
  }
  if (url.pathname === "/api/version") {
    const vf = path.join(__dirname, "..", "version.json");
    res.writeHead(200, { "Content-Type": "application/json", ...cors });
    if (fs.existsSync(vf)) return res.end(fs.readFileSync(vf));
    return res.end(JSON.stringify({ version: "0.0.0" }));
  }
  if (url.pathname === "/download/agent") {
    const exe = path.join(__dirname, "..", "agent-go", "SWRemote-Agent.exe");
    if (!fs.existsSync(exe)) { res.writeHead(404); return res.end("not found"); }
    res.writeHead(200, {
      "Content-Type": "application/octet-stream",
      "Content-Disposition": 'attachment; filename="SWRemote-Agent.exe"',
      "Content-Length": fs.statSync(exe).size,
    });
    return fs.createReadStream(exe).pipe(res);
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

// send the current viewer list (identity) to an agent
function sendViewers(rec) {
  if (!rec || rec.ws.readyState !== 1) return;
  const list = [];
  for (const v of rec.viewers) {
    const info = viewers.get(v);
    if (info) list.push({ vid: info.vid, name: info.name, avatar: info.avatar });
  }
  send(rec.ws, { t: "viewers", viewers: list });
}

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
      const prevStored = store.devices[deviceId] || {};
      store.devices[deviceId] = {
        name: rec.name, platform: rec.platform,
        pinHash: pinHash(deviceId, String(msg.pin)),
        lastSeen: rec.lastSeen,
        // claim code survives re-registers until claimed (Phase 1)
        claimCode: prevStored.claimCode || String(msg.claimCode || "").toUpperCase() || null,
      };
      saveStore();
      // re-attach existing viewers to the new socket
      for (const v of rec.viewers) { viewers.set(v, { deviceId }); send(v, { t: "agent_back", screen: rec.screen }); }
      send(ws, { t: "registered", id: deviceId, viewers: rec.viewers.size });
      sendViewers(rec);
      return;
    }

    if (msg.t === "ping" && role === "agent") {
      const rec = agents.get(deviceId);
      if (rec) rec.lastSeen = Date.now();
      return send(ws, { t: "pong" });
    }

    // ---- viewer joins a device ----
    // PIN join (guest / quick help) or token join (same account, no PIN — Phase 1)
    if (msg.t === "join") {
      const id = String(msg.id || "");
      const d = store.devices[id];
      if (!d) return send(ws, { t: "error", msg: "unknown device" });
      const finishJoin = () => {
        role = "viewer"; deviceId = id;
        const vid = "v" + Math.random().toString(36).slice(2, 10);
        viewers.set(ws, { deviceId: id, vid, name: String(msg.name || "Guest").slice(0, 40), avatar: String(msg.avatar || "").slice(0, 500) });
        const rec = agents.get(id);
        if (rec && rec.ws.readyState === 1) {
          rec.viewers.add(ws);
          send(ws, { t: "joined", id, name: d.name, screen: rec.screen, online: true, vid });
          sendViewers(rec);
        } else {
          send(ws, { t: "joined", id, name: d.name, screen: null, online: false, vid });
        }
      };
      if (msg.token && sbOn) {
        (async () => {
          const user = await sbVerify(String(msg.token));
          if (!user) return send(ws, { t: "error", msg: "sign in expired — please sign in again" });
          if (!(await sbOwns(String(msg.token), id)))
            return send(ws, { t: "error", msg: "this device is not on your account" });
          finishJoin();
        })().catch(() => send(ws, { t: "error", msg: "could not verify account" }));
        return;
      }
      if (pinHash(id, String(msg.pin || "")) !== d.pinHash)
        return send(ws, { t: "error", msg: "wrong pin" });
      return finishJoin();
    }

    // ---- viewer updates identity (name/avatar) ----
    if (msg.t === "identify" && role === "viewer") {
      const v = viewers.get(ws);
      if (v) {
        if (msg.name) v.name = String(msg.name).slice(0, 40);
        if (msg.avatar !== undefined) v.avatar = String(msg.avatar).slice(0, 500);
        const rec = agents.get(deviceId);
        if (rec) sendViewers(rec);
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
      if (rec) { rec.viewers.delete(ws); sendViewers(rec); }
    }
  });
});

httpServer.listen(PORT, () => {
  console.log(`SWRemote relay listening on :${PORT}  (ws path /ws, console at /)`);
});
