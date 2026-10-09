/* SWRemote web console v1 */
"use strict";
const $ = (id) => document.getElementById(id);
const LS_SERVER = "swr_server";

let ws = null, deviceId = null, deviceName = "", frameW = 0, frameH = 0;
let decoding = false, lastMove = 0;

/* ---------- screens ---------- */
function show(id) {
  document.querySelectorAll(".screen").forEach((s) => s.classList.remove("active"));
  $(id).classList.add("active");
}

/* ---------- server ---------- */
const DEFAULT_SERVER = "https://swremote-relay.onrender.com";
function serverBase() {
  return (localStorage.getItem(LS_SERVER) || "").replace(/\/$/, "");
}
function wsUrl() {
  return serverBase().replace(/^http/, "ws") + "/ws";
}
async function boot() {
  let saved = localStorage.getItem(LS_SERVER);
  if (!saved) {
    // first run: the relay address is built in — no typing needed
    saved = DEFAULT_SERVER;
    localStorage.setItem(LS_SERVER, saved);
  }
  $("server-url").value = saved;
  await sbInit();
  if (sbOn() && !sbSignedIn()) enterAuth();
  else enterDevices(); // also handles ?id= direct invite links
}
$("btn-save-server").onclick = async () => {
  const v = $("server-url").value.trim().replace(/\/$/, "");
  if (!v) return;
  localStorage.setItem(LS_SERVER, v);
  await sbInit();
  if (sbOn() && !sbSignedIn()) enterAuth();
  else enterDevices();
};
$("btn-change-server").onclick = () => { show("scr-login"); };

/* ---------- Supabase Auth (Phase 1: accounts) ---------- */
let sbCfg = { url: null, anonKey: null };
let sbSession = null;
try { sbSession = JSON.parse(localStorage.getItem("swr_sb") || "null"); } catch {}
const sbOn = () => !!(sbCfg.url && sbCfg.anonKey);
const sbSignedIn = () => !!(sbSession && sbSession.access_token);

async function sbInit() {
  try {
    const r = await fetch(serverBase() + "/api/config");
    const j = await r.json();
    if (j.supabaseUrl && j.supabaseAnonKey) { sbCfg.url = j.supabaseUrl; sbCfg.anonKey = j.supabaseAnonKey; }
  } catch {}
}
function sbSave(s) {
  sbSession = s;
  if (s) localStorage.setItem("swr_sb", JSON.stringify(s));
  else localStorage.removeItem("swr_sb");
}
async function sbCall(path, body, token) {
  const r = await fetch(sbCfg.url + path, {
    method: "POST",
    headers: { apikey: sbCfg.anonKey, "Content-Type": "application/json", ...(token ? { Authorization: "Bearer " + token } : {}) },
    body: JSON.stringify(body || {}),
  });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.msg || j.error_description || j.error || ("error " + r.status));
  return j;
}
let authMode = "in"; // "in" | "up"
function setAuthMode(m) {
  authMode = m;
  $("auth-title").textContent = m === "in" ? "Welcome back" : "Create account";
  $("auth-sub").textContent = m === "in" ? "Sign in to see your devices" : "One account for all your devices";
  $("btn-auth-go").textContent = m === "in" ? "Sign in" : "Create account";
  $("auth-toggle").textContent = m === "in" ? "New here? Create an account" : "Have an account? Sign in";
  $("auth-err").style.display = "none";
}
async function doAuth() {
  const email = $("auth-email").value.trim(), pw = $("auth-pass").value;
  const errEl = $("auth-err");
  errEl.style.display = "none";
  if (!email || !pw) { errEl.textContent = "Enter your email and password."; errEl.style.display = "block"; return; }
  const btn = $("btn-auth-go");
  btn.disabled = true;
  try {
    let j;
    if (authMode === "up") {
      j = await sbCall("/auth/v1/signup", { email, password: pw });
      if (j.user && !j.session) throw new Error("Check your email for a confirmation link, then sign in.");
    } else {
      j = await sbCall("/auth/v1/token?grant_type=password", { email, password: pw });
    }
    if (!j.access_token) throw new Error("Could not start a session.");
    sbSave({ access_token: j.access_token, refresh_token: j.refresh_token, expires_at: Date.now() + (j.expires_in || 3600) * 1000, email: j.user && j.user.email || email });
    toast("Signed in as " + email);
    enterDevices();
  } catch (e) {
    errEl.textContent = e.message;
    errEl.style.display = "block";
  } finally { btn.disabled = false; }
}
async function sbToken() {
  if (!sbSession) return null;
  if (Date.now() < sbSession.expires_at - 60000) return sbSession.access_token;
  try {
    const j = await sbCall("/auth/v1/token?grant_type=refresh_token", { refresh_token: sbSession.refresh_token });
    sbSave({ access_token: j.access_token, refresh_token: j.refresh_token || sbSession.refresh_token, expires_at: Date.now() + (j.expires_in || 3600) * 1000, email: sbSession.email });
    return sbSession.access_token;
  } catch { sbSave(null); return null; }
}
function sbLogout() {
  sbSave(null);
  enterAuth();
}
$("btn-auth-go").onclick = doAuth;
$("auth-toggle").onclick = (e) => { e.preventDefault(); setAuthMode(authMode === "in" ? "up" : "in"); };
$("auth-skip").onclick = (e) => { e.preventDefault(); enterDevices(); };
$("auth-pass").addEventListener("keydown", (e) => { if (e.key === "Enter") doAuth(); });

function enterAuth() { setAuthMode("in"); show("scr-auth"); }
function enterDevices() { show("scr-devices"); renderAcctRow(); loadDevices(); }
function renderAcctRow() {
  const el = $("acct-row");
  const showAcct = sbOn();
  $("btn-claim").classList.toggle("hidden", !showAcct || !sbSignedIn());
  $("manual-join").classList.toggle("hidden", !(showAcct && !sbSignedIn()));
  if (!showAcct) { el.innerHTML = ""; return; }
  el.innerHTML = sbSignedIn()
    ? `Signed in as <b>${esc(sbSession.email)}</b> — <a href="#" id="link-signout">sign out</a>`
    : `You're browsing as a guest — <a href="#" id="link-signin">sign in</a> to see your devices.`;
  const so = $("link-signout"), si = $("link-signin");
  if (so) so.onclick = (e) => { e.preventDefault(); sbLogout(); };
  if (si) si.onclick = (e) => { e.preventDefault(); enterAuth(); };
}
$("btn-claim").onclick = () => { $("claim-input").value = ""; $("claim-modal").classList.remove("hidden"); $("claim-input").focus(); };
$("claim-cancel").onclick = () => $("claim-modal").classList.add("hidden");
$("claim-ok").onclick = async () => {
  const code = $("claim-input").value.trim();
  if (!code) return;
  const token = await sbToken();
  if (!token) { enterAuth(); return; }
  const btn = $("claim-ok");
  btn.disabled = true;
  try {
    const r = await fetch(serverBase() + "/api/claim", {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + token },
      body: JSON.stringify({ claim_code: code }),
    });
    const j = await r.json();
    if (!r.ok) throw new Error(j.error || "claim failed");
    $("claim-modal").classList.add("hidden");
    toast("Device claimed: " + (j.name || j.id));
    loadDevices();
  } catch (e) { toast(e.message); }
  finally { btn.disabled = false; }
};
$("btn-manual").onclick = () => {
  const id = $("manual-id").value.trim();
  if (!id) return;
  askPin({ id, name: id });
};

/* ---------- devices ---------- */
function relTime(ts) {
  if (!ts) return "never";
  const s = Math.max(0, Math.floor((Date.now() - ts) / 1000));
  if (s < 10) return "just now";
  if (s < 60) return s + "s ago";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "m ago";
  const h = Math.floor(m / 60);
  if (h < 24) return h + "h ago";
  return Math.floor(h / 24) + "d ago";
}
let toastTimer = null;
function toast(msg) {
  document.querySelectorAll(".toast").forEach((el) => {
    el.textContent = msg; el.classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.remove("show"), 2200);
  });
}
async function loadDevices() {
  const list = $("device-list");
  list.innerHTML = '<div class="skel"><div class="sk-row"><div class="sk sk-ico"></div><div style="flex:1"><div class="sk sk-t1"></div><div class="sk sk-t2"></div></div></div><div class="sk sk-btn"></div></div>'
    + '<div class="skel"><div class="sk-row"><div class="sk sk-ico"></div><div style="flex:1"><div class="sk sk-t1"></div><div class="sk sk-t2"></div></div></div><div class="sk sk-btn"></div></div>';
  try {
    const headers = {};
    const token = sbOn() ? await sbToken() : null;
    if (token) headers.Authorization = "Bearer " + token;
    const r = await fetch(serverBase() + "/api/devices", { headers });
    const j = await r.json();
    const online = j.devices.filter((d) => d.online).length;
    $("dev-count").textContent = j.devices.length
      ? `${online} of ${j.devices.length} online` : "";
    if (!j.devices.length) {
      list.innerHTML = sbOn() && sbSignedIn()
        ? '<div class="empty"><span class="empty-ico">🖥️</span><b>No devices yet.</b><br>Tap <b>＋ Add</b> and enter the code from the PC app.</div>'
        : '<div class="empty"><span class="empty-ico">🖥️</span><b>No devices yet.</b><br>Open <b>SWRemote-Agent.exe</b> on a Windows PC first.</div>';
      return;
    }
    list.innerHTML = "";
    j.devices.forEach((d) => {
      const card = document.createElement("div");
      card.className = "dev-card" + (d.online ? "" : " offline");
      card.innerHTML = `
        <div class="dev-top">
          <div class="dev-ico">💻</div>
          <div><div class="dev-name">${esc(d.name)}</div><div class="dev-id">${esc(d.id)}</div></div>
        </div>
        <div class="dev-meta">
          <span class="pill ${d.online ? "on" : "off"}"><span class="dot ${d.online ? "on" : "off"}"></span>${d.online ? "Online" : "Offline"}</span>
          <span class="dev-seen">${d.online ? "connected now" : "seen " + relTime(d.lastSeen)}</span>
        </div>
        <div class="dev-actions">
          <button class="ghost btn-share">⤴ Share</button>
          <button class="primary btn-connect" ${d.online ? "" : "disabled"}>${d.mine ? "Open" : "Connect"}</button>
        </div>`;
      // own devices join with the account token — no PIN needed
      if (d.online) card.querySelector(".btn-connect").onclick = () => d.mine ? startSessionToken(d) : askPin(d);
      card.querySelector(".btn-share").onclick = () => shareDevice(d);
      list.appendChild(card);
    });
    // direct link support: ?id=123456789 opens the PIN box for that device
    const want = new URLSearchParams(location.search).get("id");
    if (want) {
      const target = j.devices.find((d) => d.id === want.trim());
      if (target && target.online) (target.mine ? startSessionToken(target) : askPin(target));
      else if (target) toast("That PC is offline right now.");
    }
  } catch (e) {
    $("dev-count").textContent = "";
    list.innerHTML = `<div class="empty"><span class="empty-ico">📡</span><b>Cannot reach the server.</b><br>${esc(e.message)}<br>Check your connection and tap refresh.</div>`;
  }
}
function shareDevice(d) {
  const link = location.origin + location.pathname + "?id=" + encodeURIComponent(d.id);
  const done = () => toast("Invite link copied — send it to them.");
  if (navigator.clipboard && navigator.clipboard.writeText)
    navigator.clipboard.writeText(link).then(done).catch(() => prompt("Copy this invite link:", link));
  else prompt("Copy this invite link:", link);
}
$("btn-refresh").onclick = () => {
  const b = $("btn-refresh");
  b.classList.remove("spin"); void b.offsetWidth; b.classList.add("spin");
  loadDevices();
};
function esc(s) { return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }

/* ---------- PIN modal ---------- */
let pinTarget = null;
function askPin(d) {
  pinTarget = d;
  $("pin-title").textContent = d.name;
  $("pin-devid").textContent = "ID " + d.id;
  $("pin-input").value = "";
  $("pin-modal").classList.remove("hidden");
  setTimeout(() => $("pin-input").focus(), 80);
}
$("pin-cancel").onclick = () => $("pin-modal").classList.add("hidden");
$("pin-ok").onclick = () => {
  const pin = $("pin-input").value.trim();
  if (!pin || !pinTarget) return;
  $("pin-modal").classList.add("hidden");
  startSession(pinTarget, pin);
};
$("pin-input").addEventListener("keydown", (e) => { if (e.key === "Enter") $("pin-ok").click(); });

/* ---------- session ---------- */
function startSession(d, pin) {
  startSessionWith(d, { pin });
}
async function startSessionToken(d) {
  const token = await sbToken();
  if (!token) { enterAuth(); return; }
  startSessionWith(d, { token });
}
function startSessionWith(d, creds) {
  deviceId = d.id; deviceName = d.name;
  $("sess-name").textContent = d.name;
  show("scr-session");
  setStatus("Connecting…");
  closeWs();
  ws = new WebSocket(wsUrl());
  ws.binaryType = "arraybuffer";
  ws.onopen = () => ws.send(JSON.stringify({ t: "join", id: deviceId, ...creds }));
  ws.onmessage = onWsMsg;
  ws.onclose = () => setStatus("Disconnected.", true);
  ws.onerror = () => setStatus("Connection error.", true);
  bindCanvasInput();
  ptrs.clear(); gesture = null; gestureActive = false;
  resetZoom();
}
function closeWs() { try { ws && ws.close(); } catch {} ws = null; }
$("btn-disconnect").onclick = () => { closeWs(); enterDevices(); };
function setStatus(txt, err) {
  const el = $("sess-status");
  el.textContent = txt;
  el.classList.toggle("err", !!err);
  el.style.display = txt ? "block" : "none";
}

const BIN_FRAME = 1, BIN_FILE = 2, BIN_TILE = 3;
const TILE = 128;

function onWsMsg(ev) {
  if (ev.data instanceof ArrayBuffer) { handleBinary(new Uint8Array(ev.data)); return; }
  let m; try { m = JSON.parse(ev.data); } catch { return; }
  if (m.t === "joined") {
    if (!m.online) { setStatus("Device is offline.", true); return; }
    setStatus(""); $("sess-dot").className = "dot on";
    if (m.screen) sizeCanvas(m.screen.w, m.screen.h);
    ws.send(JSON.stringify({ t: "keyframe" })); // ask for a full frame now
  }
  else if (m.t === "error") setStatus("Error: " + (m.msg || "unknown"), true);
  else if (m.t === "agent_gone") { setStatus("Device went offline.", true); $("sess-dot").className = "dot off"; }
  else if (m.t === "agent_back") { setStatus(""); $("sess-dot").className = "dot on"; }
  else if (m.t === "chat") addChat(m.text, false);
  else if (m.t === "cursor") moveRemoteCursor(m.x, m.y);
  else if (m.t === "file_meta") noteFileFromAgent(m);
}

function handleBinary(u8) {
  const kind = u8[0];
  if (kind === BIN_FRAME) { drawFrame(u8.subarray(1)); return; }
  if (kind === BIN_TILE) { queueTile(u8); return; }
  if (kind === BIN_FILE) { recvFileChunk(u8.subarray(1)); return; }
}

const canvas = $("screen"), ctx = canvas.getContext("2d");
function sizeCanvas(w, h) { frameW = w; frameH = h; canvas.width = w; canvas.height = h; fitCanvas(true); }
let lastStageW = 0, lastStageH = 0;
let zoom = 1, fitScale = 1;
function stageEl() { return document.querySelector(".stage"); }
function applyZoomSize() {
  if (!frameW || !fitScale) return;
  canvas.style.width = Math.max(1, Math.floor(frameW * fitScale * zoom)) + "px";
  canvas.style.height = Math.max(1, Math.floor(frameH * fitScale * zoom)) + "px";
}
function fitCanvas(force) {
  const stage = stageEl();
  const r = stage.getBoundingClientRect();
  // skip tiny/zero layouts (page still settling) and repeat work
  if (r.width < 10 || r.height < 10) return;
  if (!force && Math.abs(r.width - lastStageW) < 2 && Math.abs(r.height - lastStageH) < 2) return;
  lastStageW = r.width; lastStageH = r.height;
  if (!frameW) return;
  fitScale = Math.min(r.width / frameW, r.height / frameH);
  applyZoomSize();
}
function zoomTo(z, vx, vy) {
  const st = stageEl();
  const r = st.getBoundingClientRect();
  const ax = (vx === undefined ? r.width / 2 : vx - r.left);
  const ay = (vy === undefined ? r.height / 2 : vy - r.top);
  const fx = (st.scrollLeft + ax) / Math.max(1, st.scrollWidth);
  const fy = (st.scrollTop + ay) / Math.max(1, st.scrollHeight);
  zoom = Math.min(4, Math.max(1, z));
  applyZoomSize();
  st.scrollLeft = fx * st.scrollWidth - ax;
  st.scrollTop = fy * st.scrollHeight - ay;
  const lbl = $("zoom-label");
  if (lbl) lbl.textContent = Math.round(zoom * 100) + "%";
}
function resetZoom() {
  zoom = 1; applyZoomSize();
  const st = stageEl(); st.scrollLeft = 0; st.scrollTop = 0;
  const lbl = $("zoom-label");
  if (lbl) lbl.textContent = "100%";
}
window.addEventListener("resize", () => fitCanvas(true));

async function drawFrame(jpeg) {
  if (decoding) return; // drop frames while busy (keeps latency low)
  decoding = true;
  try {
    const bmp = await createImageBitmap(new Blob([jpeg], { type: "image/jpeg" }));
    if (canvas.width !== bmp.width || canvas.height !== bmp.height) sizeCanvas(bmp.width, bmp.height);
    ctx.drawImage(bmp, 0, 0);
    bmp.close();
    fitCanvas(); // re-fit if the layout settled after the first frames
    if ($("sess-status").textContent) setStatus("");
    framesThisSec++;
  } catch {} finally { decoding = false; }
}

// live FPS pill
let framesThisSec = 0;
setInterval(() => {
  const el = $("sess-fps");
  if ($("scr-session").classList.contains("active") && deviceId) {
    el.textContent = framesThisSec + " fps";
    el.style.display = framesThisSec ? "inline-block" : "none";
  } else el.style.display = "none";
  framesThisSec = 0;
}, 2000);

// ---- tiled rendering (v3.0): tiles are composited straight onto the canvas.
// Tiles arrive as: 0x03 + uint16BE idx + JPEG. idx = row*cols + col.
let tileQueue = Promise.resolve();
function queueTile(u8) {
  tileQueue = tileQueue.then(() => drawTile(u8)).catch(() => {});
}
async function drawTile(u8) {
  if (!frameW) return; // no keyframe yet — will come shortly
  const idx = (u8[1] << 8) | u8[2];
  const cols = Math.ceil(frameW / TILE);
  const col = idx % cols, row = Math.floor(idx / cols);
  const bmp = await createImageBitmap(new Blob([u8.subarray(3)], { type: "image/jpeg" }));
  ctx.drawImage(bmp, col * TILE, row * TILE);
  bmp.close();
  framesThisSec++;
  if ($("sess-status").textContent) setStatus("");
}

// ---- remote cursor overlay (v3.0) ----
function moveRemoteCursor(x, y) {
  const cur = $("cursor");
  if (!frameW || !canvas.clientWidth) return;
  const s = canvas.clientWidth / frameW;
  cur.style.display = "block";
  cur.style.transform = `translate(${Math.round(x * s)}px, ${Math.round(y * s)}px)`;
  clearTimeout(cur._t);
  cur._t = setTimeout(() => (cur.style.display = "none"), 4000);
}

// fullscreen toggle
$("btn-full").onclick = () => {
  const stage = document.querySelector(".stage");
  if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
  else if (stage.requestFullscreen) stage.requestFullscreen().catch(() => {});
  setTimeout(() => fitCanvas(true), 300);
};
document.addEventListener("fullscreenchange", () => setTimeout(() => fitCanvas(true), 200));

/* ---------- pinch zoom + two-finger pan (mobile) ---------- */
let gesture = null, gestureActive = false;
const ptrs = new Map();
function bindGestures() {
  const st = stageEl();
  st.addEventListener("pointerdown", (e) => {
    ptrs.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (ptrs.size === 2) {
      const [a, b] = [...ptrs.values()];
      gesture = {
        startDist: Math.hypot(a.x - b.x, a.y - b.y) || 1,
        startZoom: zoom,
        prevMid: { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 },
        t0: Date.now(), maxMove: 0,
      };
      gestureActive = true;
    }
  });
  st.addEventListener("pointermove", (e) => {
    if (!ptrs.has(e.pointerId)) return;
    ptrs.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (gesture && ptrs.size >= 2) {
      const [a, b] = [...ptrs.values()];
      const dist = Math.hypot(a.x - b.x, a.y - b.y) || 1;
      const mid = { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
      const move = Math.hypot(mid.x - gesture.prevMid.x, mid.y - gesture.prevMid.y);
      gesture.maxMove = Math.max(gesture.maxMove, move, Math.abs(dist - gesture.startDist));
      st.scrollLeft -= (mid.x - gesture.prevMid.x);
      st.scrollTop -= (mid.y - gesture.prevMid.y);
      const nz = gesture.startZoom * (dist / gesture.startDist);
      if (Math.abs(nz - zoom) > 0.02) zoomTo(nz, mid.x, mid.y);
      gesture.prevMid = mid;
      e.preventDefault();
    }
  });
  const endPtr = (e) => {
    ptrs.delete(e.pointerId);
    if (gesture && ptrs.size === 0) {
      // quick two-finger tap = right click
      if (Date.now() - gesture.t0 < 350 && gesture.maxMove < 12) {
        const p = rel({ clientX: gesture.prevMid.x, clientY: gesture.prevMid.y });
        sendInput({ k: "move", ...p });
        sendInput({ k: "down", b: "right" });
        sendInput({ k: "up", b: "right" });
      }
      gesture = null;
    }
    if (ptrs.size < 2) gestureActive = false;
  };
  st.addEventListener("pointerup", endPtr);
  st.addEventListener("pointercancel", endPtr);
  $("zoom-in").onclick = () => zoomTo(zoom * 1.3);
  $("zoom-out").onclick = () => zoomTo(zoom / 1.3);
  $("zoom-label").onclick = resetZoom;
}

/* ---------- input: mouse + touch ---------- */
function rel(e) {
  const r = canvas.getBoundingClientRect();
  return {
    x: Math.min(1, Math.max(0, (e.clientX - r.left) / r.width)),
    y: Math.min(1, Math.max(0, (e.clientY - r.top) / r.height)),
  };
}
function sendInput(o) { if (ws && ws.readyState === 1) ws.send(JSON.stringify({ t: "input", ...o })); }

let touchState = null;
function bindCanvasInput() {
  canvas.onpointerdown = (e) => {
    if (gestureActive) return;
    canvas.setPointerCapture(e.pointerId);
    const p = rel(e);
    if (e.pointerType === "touch") { touchState = { x: e.clientX, y: e.clientY, moved: false, t: Date.now(), two: false }; }
    sendInput({ k: "move", ...p });
    const btn = e.button === 2 ? "right" : e.button === 1 ? "middle" : "left";
    if (e.pointerType !== "touch") sendInput({ k: "down", b: btn });
    e.preventDefault();
  };
  canvas.onpointermove = (e) => {
    if (gestureActive) return;
    if (touchState && Math.hypot(e.clientX - touchState.x, e.clientY - touchState.y) > 12) touchState.moved = true;
    const now = performance.now();
    if (now - lastMove < 40) return;
    lastMove = now;
    sendInput({ k: "move", ...rel(e) });
  };
  canvas.onpointerup = (e) => {
    if (gestureActive) return;
    const p = rel(e);
    if (e.pointerType === "touch" && touchState) {
      const dt = Date.now() - touchState.t;
      if (!touchState.moved && dt < 400) { // tap = left click
        sendInput({ k: "move", ...p });
        sendInput({ k: "down", b: "left" });
        sendInput({ k: "up", b: "left" });
      }
      touchState = null;
    } else {
      sendInput({ k: "move", ...p });
      const btn = e.button === 2 ? "right" : e.button === 1 ? "middle" : "left";
      sendInput({ k: "up", b: btn });
    }
  };
  canvas.onpointercancel = () => { touchState = null; };
  canvas.oncontextmenu = (e) => e.preventDefault();
  canvas.onwheel = (e) => { sendInput({ k: "scroll", dx: 0, dy: Math.sign(e.deltaY) * -3 }); e.preventDefault(); };
}

/* ---------- keyboard ---------- */
$("btn-kbd").onclick = () => {
  $("keybar").classList.toggle("hidden");
  const kp = $("kbd-proxy");
  kp.value = "";
  kp.focus({ preventScroll: true });
  fitCanvas();
};
document.querySelectorAll("#keybar button").forEach((b) => {
  const key = b.dataset.key;
  b.addEventListener("pointerdown", (e) => { e.preventDefault(); sendInput({ k: "key", key, down: true }); });
  b.addEventListener("pointerup", () => sendInput({ k: "key", key, down: false }));
});
$("kbd-proxy").addEventListener("keydown", (e) => {
  const map = { Enter: "enter", Tab: "tab", Backspace: "backspace", Delete: "delete", Escape: "esc", ArrowUp: "up", ArrowDown: "down", ArrowLeft: "left", ArrowRight: "right", Home: "home", End: "end", PageUp: "pageup", PageDown: "pagedown" };
  if (map[e.key]) { sendInput({ k: "key", key: map[e.key], down: true }); setTimeout(() => sendInput({ k: "key", key: map[e.key], down: false }), 60); e.preventDefault(); }
  else if (e.key.length === 1 && !e.ctrlKey && !e.metaKey) { sendInput({ k: "type", text: e.key }); $("kbd-proxy").value = ""; e.preventDefault(); }
  else if ((e.ctrlKey || e.metaKey) && e.key.length === 1) { // ctrl combos
    sendInput({ k: "key", key: "ctrl", down: true });
    sendInput({ k: "key", key: e.key.toLowerCase(), down: true });
    setTimeout(() => sendInput({ k: "key", key: e.key.toLowerCase(), down: false }), 60);
    setTimeout(() => sendInput({ k: "key", key: "ctrl", down: false }), 120);
    e.preventDefault();
  }
});

/* ---------- chat ---------- */
$("btn-chat").onclick = () => toggleDrawer("chat-panel");
document.querySelectorAll("[data-close]").forEach((b) => b.onclick = () => $(b.dataset.close).classList.add("hidden"));
function toggleDrawer(id) {
  const el = $(id), wasHidden = el.classList.contains("hidden");
  document.querySelectorAll(".sheet").forEach((d) => d.classList.add("hidden"));
  if (wasHidden) el.classList.remove("hidden");
}
function addChat(text, me) {
  const div = document.createElement("div");
  div.className = "chat-msg " + (me ? "me" : "them");
  div.textContent = text;
  $("chat-log").appendChild(div);
  $("chat-log").scrollTop = 1e6;
}
$("chat-send").onclick = sendChat;
$("chat-text").addEventListener("keydown", (e) => { if (e.key === "Enter") sendChat(); });
function sendChat() {
  const t = $("chat-text").value.trim();
  if (!t || !ws) return;
  ws.send(JSON.stringify({ t: "chat", text: t }));
  addChat(t, true);
  $("chat-text").value = "";
}

/* ---------- files: console -> agent ---------- */
let pendingFile = null;
$("btn-files").onclick = () => toggleDrawer("files-panel");
$("file-input").onchange = (e) => { pendingFile = e.target.files[0] || null; $("btn-send-file").disabled = !pendingFile; };
$("btn-send-file").onclick = async () => {
  if (!pendingFile || !ws) return;
  const fid = "f" + Date.now().toString(36);
  const idBytes = new TextEncoder().encode(fid);
  ws.send(JSON.stringify({ t: "file_meta", id: fid, name: pendingFile.name, size: pendingFile.size }));
  const CHUNK = 48 * 1024;
  let sent = 0;
  const prog = $("file-progress");
  while (sent < pendingFile.size) {
    const slice = pendingFile.slice(sent, sent + CHUNK);
    const buf = new Uint8Array(await slice.arrayBuffer());
    const pkt = new Uint8Array(2 + idBytes.length + buf.length);
    pkt[0] = BIN_FILE; pkt[1] = idBytes.length;
    pkt.set(idBytes, 2); pkt.set(buf, 2 + idBytes.length);
    ws.send(pkt);
    sent += buf.length;
    prog.textContent = `Sending… ${Math.round((sent / pendingFile.size) * 100)}%`;
    await new Promise((r) => setTimeout(r, 10));
  }
  prog.textContent = "Sent ✓  (check Downloads\\SWRemote on the PC)";
};

/* ---------- files: agent -> console (download) ---------- */
let dlFile = null;
function noteFileFromAgent(m) {
  dlFile = { id: m.id, name: m.name || "file.bin", size: m.size || 0, parts: [], got: 0 };
  addChat(`📥 Receiving "${dlFile.name}"…`, false);
}
function recvFileChunk(u8) {
  if (!dlFile) return;
  const idLen = u8[0];
  const fid = new TextDecoder().decode(u8.subarray(1, 1 + idLen));
  if (fid !== dlFile.id) return;
  dlFile.parts.push(u8.subarray(1 + idLen));
  dlFile.got += u8.length - 1 - idLen;
  if (dlFile.got >= dlFile.size) {
    const blob = new Blob(dlFile.parts);
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = dlFile.name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 5000);
    addChat(`📥 "${dlFile.name}" downloaded.`, false);
    dlFile = null;
  }
}

/* ---------- quality / lock ---------- */
$("btn-quality").onclick = () => $("quality-pop").classList.toggle("hidden");
function pushQuality() {
  if (!ws) return;
  ws.send(JSON.stringify({ t: "quality", q: +$("q-quality").value, scale: +$("q-scale").value }));
}
$("q-quality").onchange = pushQuality;
$("q-scale").onchange = pushQuality;
$("btn-lock").onclick = () => { if (ws && confirm("Lock the remote PC now?")) ws.send(JSON.stringify({ t: "lock" })); };

/* ---------- PWA ---------- */
if ("serviceWorker" in navigator) navigator.serviceWorker.register("sw.js").catch(() => {});
let deferredInstall = null;
window.addEventListener("beforeinstallprompt", (e) => {
  e.preventDefault();
  deferredInstall = e;
  $("btn-install").classList.remove("hidden");
});
$("btn-install").onclick = async () => {
  if (!deferredInstall) return;
  deferredInstall.prompt();
  await deferredInstall.userChoice.catch(() => {});
  deferredInstall = null;
  $("btn-install").classList.add("hidden");
};
bindGestures();
boot();
