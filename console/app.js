/* SWRemote web console v1 */
"use strict";
const $ = (id) => document.getElementById(id);
const LS_SERVER = "swr_server";

let ws = null, deviceId = null, deviceName = "", frameW = 0, frameH = 0;
let decoding = false, lastMove = 0;

/* ---------- screens ---------- */
function show(id) {
  ["scr-auth", "scr-app"].forEach((s) => $(s).classList.toggle("hidden", s !== id));
  $("scr-session").classList.toggle("active", id === "scr-session");
  window.scrollTo(0, 0);
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
  await sbInit();
  if (sbOn() && sbSignedIn()) await loadProfile();
  if (sbOn() && !sbSignedIn()) enterAuth();
  else enterDevices(); // also handles ?id= direct invite links
}
/* ---------- Dashboard navigation (Stitch edition) ---------- */
function navTo(page) {
  document.querySelectorAll("#side-nav button, #mobile-nav button").forEach((b) => {
    const on = b.dataset.page === page;
    b.classList.toggle("is-on", on);
    if (on) b.setAttribute("aria-current", "page"); else b.removeAttribute("aria-current");
  });
  ["devices", "downloads", "account"].forEach((p) => $("page-" + p).classList.toggle("hidden", p !== page));
  if (page === "downloads") loadDownloadMeta();
  if (page === "account") renderAccountPage();
  window.scrollTo(0, 0);
}
document.querySelectorAll("#side-nav button, #mobile-nav button").forEach((b) => { b.onclick = () => navTo(b.dataset.page); });
$("dev-search").addEventListener("input", () => renderDeviceList(lastDevices));

async function loadDownloadMeta() {
  try {
    const v = await (await fetch(serverBase() + "/api/version")).json();
    $("dl-version").textContent = v.version ? "Current release: v" + v.version : "";
  } catch {}
  for (const [kind, el] of [["agent", "dl-agent-meta"], ["setup", "dl-setup-meta"]]) {
    try {
      const r = await fetch(serverBase() + "/download/" + kind, { method: "HEAD" });
      const len = r.headers.get("content-length");
      $(el).textContent = r.ok && len ? "Windows 64-bit · " + (len / 1048576).toFixed(1) + " MB" : "Windows 64-bit";
    } catch { $(el).textContent = "Windows 64-bit"; }
  }
}

let myProfile = { display_name: "", avatar_url: "" };
async function loadProfile() {
  myProfile = { display_name: "", avatar_url: "" };
  if (!sbOn() || !sbSignedIn()) return;
  try {
    const token = await sbToken();
    const uid = sbUserId();
    if (!token || !uid) return;
    // v10.1: filter to MY row — the unfiltered query took row [0], which
    // could be somebody else's profile entirely.
    const j = await sbCall("/rest/v1/profiles?id=eq." + uid + "&select=display_name,avatar_url", null, { Authorization: "Bearer " + token }, "GET");
    if (Array.isArray(j) && j[0]) myProfile = { display_name: j[0].display_name || "", avatar_url: j[0].avatar_url || "" };
  } catch {}
}
function avatarHTML(size) {
  const box = `width:${size}px;height:${size}px`;
  if (myProfile.avatar_url) return `<img src="${esc(myProfile.avatar_url)}" class="avatar" style="${box}" alt="">`;
  const ch = (myProfile.display_name || sbSession?.email || "?")[0].toUpperCase();
  return `<div class="avatar" style="${box};font-size:${Math.round(size * 0.42)}px">${esc(ch)}</div>`;
}
// v10.1: phone photos are 3–8 MB — shrink to ≤512px JPEG before upload so
// it finishes fast even on a hotspot, and the stored avatar stays light.
async function shrinkImage(file) {
  const origExt = (file.name.split(".").pop() || "jpg").toLowerCase().replace(/[^a-z]/g, "") || "jpg";
  try {
    const bmp = await createImageBitmap(file);
    const max = 512;
    const scale = Math.min(1, max / Math.max(bmp.width, bmp.height));
    if (scale === 1 && file.size < 400 * 1024) { bmp.close && bmp.close(); return { blob: file, ext: origExt }; }
    const cv = document.createElement("canvas");
    cv.width = Math.max(1, Math.round(bmp.width * scale));
    cv.height = Math.max(1, Math.round(bmp.height * scale));
    cv.getContext("2d").drawImage(bmp, 0, 0, cv.width, cv.height);
    bmp.close && bmp.close();
    const blob = await new Promise((res) => cv.toBlob(res, "image/jpeg", 0.85));
    if (blob) return { blob, ext: "jpg" };
  } catch {}
  return { blob: file, ext: origExt };
}
async function uploadAvatar(file) {
  const token = await sbToken();
  const uid = sbUserId();
  if (!token || !uid) throw new Error("Sign in required");
  if (!/^image\//.test(file.type || "")) throw new Error("Please choose an image file");
  const { blob, ext } = await shrinkImage(file);
  const path = `${uid}/avatar.${ext}`;
  let up;
  try {
    up = await fetch(sbUrl() + "/storage/v1/object/avatars/" + path, {
      method: "POST", headers: { Authorization: "Bearer " + token, "Content-Type": ext === "jpg" ? "image/jpeg" : (file.type || "image/jpeg"), "x-upsert": "true" },
      body: blob,
    });
  } catch (e) { throw new Error("Network error: " + e.message); }
  if (!up.ok) {
    const txt = await up.text().catch(() => "");
    throw new Error(`Upload failed (${up.status}): ${txt.slice(0, 200) || up.statusText}`);
  }
  const url = sbUrl() + "/storage/v1/object/public/avatars/" + path + "?t=" + Date.now();
  await sbCall("/rest/v1/profiles?id=eq." + uid, { avatar_url: url }, { Authorization: "Bearer " + token, "Content-Type": "application/json" }, "PATCH");
  myProfile.avatar_url = url;
  renderSideUser(); renderAccountPage();
  toast("Profile picture updated");
}

function renderAccountPage() {
  const c = $("account-card");
  const signed = sbOn() && sbSignedIn();
  const dname = myProfile.display_name || (signed ? sbSession.email.split("@")[0] : "");
  c.innerHTML = `
    <div class="me-cover"></div>
    <div class="me-id">
      <div class="me-av">
        ${avatarHTML(108)}
        ${signed ? `<label class="me-cam" title="Change picture" aria-label="Change picture">
          <span class="mi">photo_camera</span>
          <input type="file" id="avatar-file" accept="image/*" class="hidden">
        </label>` : ""}
      </div>
      ${signed
        ? `<input id="acct-name" class="auth-input me-name" value="${esc(dname)}" placeholder="Your name" aria-label="Your name">`
        : `<h2 class="me-name-static">Guest</h2>`}
      <p class="me-mail">${signed ? esc(sbSession.email) : "Browsing without an account"}</p>
      ${signed && myProfile.avatar_url ? `<span class="me-tag"><span class="mi">verified</span>Profile complete</span>` : ""}
    </div>

    <div class="me-actions">
      ${signed
        ? `<button id="btn-save-profile" class="btn btn--volt btn--block">Save profile</button>
           <button id="btn-signout2" class="btn btn--ghost btn--block">Sign out</button>`
        : `<button id="btn-signin2" class="btn btn--volt btn--block">Sign in / create account</button>`}
    </div>

    <div class="group">
      <div class="group-row">
        <label class="group-label" for="acct-server">Relay server</label>
        <div class="group-line">
          <input id="acct-server" class="auth-input" value="${esc(serverBase())}" autocapitalize="off" spellcheck="false">
          <button id="btn-acct-server" class="btn btn--soft">Save</button>
        </div>
      </div>
    </div>

    <button id="btn-install2" class="btn btn--soft btn--block me-install hidden"><span class="mi">install_mobile</span><span>Install SWRemote app</span></button>
    <p class="me-foot">SWRemote</p>`;

  if (signed) {
    $("btn-signout2").onclick = sbLogout;
    $("btn-save-profile").onclick = async () => {
      const nm = $("acct-name").value.trim();
      if (!nm) { toast("Enter your name"); return; }
      try {
        const token = await sbToken();
        await sbCall("/rest/v1/profiles?id=eq." + sbUserId(), { display_name: nm },
          { Authorization: "Bearer " + token }, "PATCH");
        myProfile.display_name = nm;
        renderSideUser(); toast("Profile saved");
      } catch { toast("Could not save"); }
    };
    $("avatar-file").onchange = async (e) => {
      const f = e.target.files[0];
      if (!f) return;
      const av = document.querySelector(".me-av");
      if (av) av.classList.add("uploading");
      try { await uploadAvatar(f); } // success re-renders the page (fresh .me-av)
      catch (err) {
        toast(err.message || "Could not upload picture");
        const av2 = document.querySelector(".me-av");
        if (av2) av2.classList.remove("uploading");
      }
    };
  } else $("btn-signin2").onclick = enterAuth;
  $("btn-acct-server").onclick = async () => {
    const v = $("acct-server").value.trim().replace(/\/$/, "");
    if (!v) return;
    localStorage.setItem(LS_SERVER, v);
    await sbInit();
    toast("Server updated");
    renderAccountPage(); renderSideUser();
  };
  if (deferredInstall) $("btn-install2").classList.remove("hidden");
  $("btn-install2").onclick = async () => {
    if (!deferredInstall) return;
    deferredInstall.prompt();
    await deferredInstall.userChoice.catch(() => {});
    deferredInstall = null;
    $("btn-install2").classList.add("hidden");
  };
}

function renderSideUser() {
  const signed = sbOn() && sbSignedIn();
  const av = $("side-avatar");
  if (signed && myProfile.avatar_url) {
    av.innerHTML = `<img src="${esc(myProfile.avatar_url)}" alt="">`;
  } else {
    av.textContent = signed ? (myProfile.display_name || sbSession.email || "S")[0].toUpperCase() : "?";
  }
  $("side-user-name").textContent = signed ? (myProfile.display_name || sbSession.email) : "Guest";
  $("side-user-sub").textContent = signed ? "Signed in" : "Not signed in";
}

/* ---------- Supabase Auth (Phase 1: accounts) ---------- */
let sbCfg = { url: null, anonKey: null };
let sbSession = null;
try { sbSession = JSON.parse(localStorage.getItem("swr_sb") || "null"); } catch {}
const sbOn = () => !!(sbCfg.url && sbCfg.anonKey);
const sbUrl = () => sbCfg.url;
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
async function sbCall(path, body, extra, method) {
  const r = await fetch(sbCfg.url + path, {
    method: method || "POST",
    headers: { apikey: sbCfg.anonKey, "Content-Type": "application/json", ...(extra || {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.msg || j.error_description || j.error || ("error " + r.status));
  return j;
}
function sbUserId() {
  try {
    const p = JSON.parse(atob(sbSession.access_token.split(".")[1]));
    return p.sub || "";
  } catch { return ""; }
}
let authMode = "in"; // "in" | "up"
function setAuthMode(m) {
  authMode = m;
  $("auth-title").textContent = m === "in" ? "Welcome back" : "Create account";
  $("auth-sub").textContent = m === "in" ? "Sign in to see your devices" : "One account for all your devices";
  $("btn-auth-go").textContent = m === "in" ? "Sign in" : "Create account";
  $("auth-toggle-text").textContent = m === "in" ? "New here? " : "Have an account? ";
  $("auth-toggle").textContent = m === "in" ? "Create an account" : "Sign in";
  $("auth-name").classList.toggle("hidden", m !== "up");
  $("auth-err").classList.add("hidden");
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
      const name = $("auth-name").value.trim();
      if (!name) { errEl.textContent = "Please enter your name."; errEl.style.display = "block"; btn.disabled = false; return; }
      j = await sbCall("/auth/v1/signup", { email, password: pw, data: { display_name: name } });
      if (j.user && !j.session) throw new Error("Check your email for a confirmation link, then sign in.");
    } else {
      j = await sbCall("/auth/v1/token?grant_type=password", { email, password: pw });
    }
    if (!j.access_token) throw new Error("Could not start a session.");
    sbSave({ access_token: j.access_token, refresh_token: j.refresh_token, expires_at: Date.now() + (j.expires_in || 3600) * 1000, email: j.user && j.user.email || email });
    await loadProfile();
    toast("Signed in as " + (myProfile.display_name || email));
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
function enterDevices() { show("scr-app"); renderSideUser(); navTo("devices"); renderAcctRow(); loadDevices(); }
function renderAcctRow() {
  const el = $("acct-row");
  const showAcct = sbOn();
  // always show Add device — it prompts sign-in if needed (btn-claim onclick handles it)
  const hideClaim = false;
  $("btn-claim").classList.toggle("hidden", hideClaim);
  $("btn-claim").classList.toggle("flex", !hideClaim);
  $("manual-join").classList.toggle("hidden", !(showAcct && !sbSignedIn()));
  if (!showAcct) { el.innerHTML = ""; return; }
  el.innerHTML = sbSignedIn()
    ? `Signed in as <b>${esc(sbSession.email)}</b> — manage everything from the <b>Account</b> tab, or <a href="#" id="link-signout" class="link">sign out</a>.`
    : `You're browsing as a guest — <a href="#" id="link-signin" class="link">sign in</a> to see your devices.`;
  const so = $("link-signout"), si = $("link-signin");
  if (so) so.onclick = (e) => { e.preventDefault(); sbLogout(); };
  if (si) si.onclick = (e) => { e.preventDefault(); enterAuth(); };
  renderSideUser();
}
$("btn-claim").onclick = () => {
  if (!sbSignedIn()) { enterAuth(); toast("Sign in first, then add your device."); return; }
  $("claim-input").value = ""; $("claim-modal").classList.remove("hidden"); $("claim-input").focus();
};
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
let lastDevices = [];
async function loadDevices(quiet) {
  const list = $("device-list");
  if (!quiet) list.innerHTML = '<div class="dev-skel"></div><div class="dev-skel"></div>';
  try {
    const headers = {};
    const token = sbOn() ? await sbToken() : null;
    if (token) headers.Authorization = "Bearer " + token;
    const r = await fetch(serverBase() + "/api/devices", { headers });
    const j = await r.json();
    loadDevices._retried = false;
    lastDevices = j.devices || [];
    renderDeviceList(lastDevices);
    const srt = $("side-relay-text");
    if (srt) { srt.textContent = "Connected"; const rp = $("relay-pill"); if (rp) rp.dataset.state = "on"; }
    // direct link support: ?id=123456789 opens the PIN box for that device (fresh loads only)
    const want = new URLSearchParams(location.search).get("id");
    if (want) {
      const target = lastDevices.find((d) => d.id === want.trim());
      if (target && target.online) (target.mine ? startSessionToken(target) : askPin(target));
      else if (target) toast("That PC is offline right now.");
    }
  } catch (e) {
    // relay asleep (free tier)? wake it and retry once instead of an error card
    if (!quiet && !loadDevices._retried) {
      loadDevices._retried = true;
      $("dev-count").textContent = "";
      list.innerHTML = stateCard("bedtime", "Waking the server…", "Free servers nap when idle — give it up to a minute.");
      wakeServerLite().then(() => loadDevices(true));
      return;
    }
    $("dev-count").textContent = "";
    list.innerHTML = stateCard("cloud_off", "Cannot reach the server.", esc(e.message) + "<br>Check your connection and tap refresh.");
  }
}
// light wake-up used outside a session: just poll health (no status text)
async function wakeServerLite() {
  for (let i = 0; i < 10; i++) {
    try {
      const r = await fetch(serverBase() + "/api/health", { cache: "no-store" });
      if (r.ok) return true;
    } catch {}
    await new Promise((res) => setTimeout(res, 4000));
  }
  return false;
}
/* ---------- 3. shared state-card + tint helpers ---------- */
function stateCard(icon, title, body, extra) {
  return `<div class="state">
    <span class="state-art"><span class="mi">${icon}</span></span>
    <h3 class="state-title">${title}</h3>
    <p class="state-body">${body}</p>${extra || ""}
  </div>`;
}
const DEV_TINTS = ["sky", "peach", "lilac", "pink", "mint", "zest"];
function devTint(id) {
  let h = 0;
  for (const c of String(id)) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return DEV_TINTS[h % DEV_TINTS.length];
}

function renderDeviceList(devices) {
  const list = $("device-list");
  const online = devices.filter((d) => d.online).length;
  $("dev-count").textContent = devices.length ? `${online} of ${devices.length} online` : "";

  if (!devices.length) {
    list.innerHTML = sbOn() && sbSignedIn()
      ? stateCard("computer", "No devices yet.",
          "Your PCs show up here once you link them. It takes under a minute.",
          `<ol class="mini-steps">
             <li><span>Run <b>SWRemote-Setup.exe</b> on your Windows PC</span></li>
             <li><span>Tap <b>Add device</b> and enter the link code</span></li>
             <li><span>Tap your PC to take control from anywhere</span></li>
           </ol>`)
      : stateCard("computer", "No devices yet.",
          "Open <b>SWRemote-Agent.exe</b> on a Windows PC first, then sign in to link it.");
    return;
  }

  const q = ($("dev-search").value || "").trim().toLowerCase();
  const shown = q ? devices.filter((d) => (d.name + " " + d.id).toLowerCase().includes(q)) : devices;
  if (!shown.length) {
    list.innerHTML = stateCard("search_off", "No matches.", "Try a different name or ID.");
    return;
  }

  // keep UI state across the auto-refresh rebuild
  const openEl = list.querySelector(".dev-more[open]");
  const openId = openEl ? openEl.dataset.id : null;
  const animate = !list.querySelector(".dev"); // entrance animation only on first paint
  list.innerHTML = "";

  // online first, then service-on, then the rest (stable)
  const ordered = shown.slice().sort((a, b) =>
    (Number(!!b.online) - Number(!!a.online)) || (Number(!!b.serviceOnline) - Number(!!a.serviceOnline)));

  ordered.forEach((d, i) => {
    const state = d.online ? "on" : d.serviceOnline ? "svc" : "off";
    const label = d.online ? "Online" : d.serviceOnline ? "Service on" : "Offline";
    const canWake = !d.online && d.serviceOnline && d.mine;

    const card = document.createElement("article");
    card.className = `dev dev--${state}` + (animate ? " dev--in" : "");
    card.dataset.id = d.id;
    card.style.setProperty("--tint", `var(--${devTint(d.id)})`);
    if (animate) card.style.setProperty("--i", Math.min(i, 8));

    const menu = d.mine
      ? `<details class="dev-more" data-id="${esc(d.id)}">
           <summary aria-label="More actions for ${esc(d.name)}"><span class="mi">more_horiz</span></summary>
           <div class="tray">
             ${d.online ? `<button class="btn-logs" type="button"><span class="mi">description</span><span>View PC logs</span></button>` : ""}
             <button class="btn-remove danger" type="button"><span class="mi">delete</span><span>Remove device</span></button>
           </div>
         </details>`
      : "";

    const primary = canWake
      ? `<button class="btn btn--mint btn-wake" type="button"><span class="mi">power_settings_new</span><span>Wake</span></button>`
      : `<button class="btn btn--volt btn-connect" type="button" ${d.online ? "" : "disabled"}><span class="mi">${d.mine ? "play_arrow" : "lock"}</span><span>${d.mine ? "Open" : "Connect"}</span></button>`;

    // v10.3: calm row layout (Account-page style) — icon chip, name/meta,
    // status pill; actions on their own line. No more monitor illustration.
    card.innerHTML = `
      <div class="dev-main">
        <span class="dev-ico" aria-hidden="true"><span class="mi">${d.online ? "desktop_windows" : "bedtime"}</span></span>
        <div class="dev-info">
          <h3 class="dev-name">${esc(d.name)}</h3>
          <p class="dev-meta"><span class="dev-id">#${esc(d.id)}</span><span class="dev-seen">${d.online ? "Active now" : "Seen " + relTime(d.lastSeen)}</span></p>
        </div>
        <span class="dev-status">${label}</span>
      </div>
      <div class="dev-actions">
        ${primary}
        <button class="btn btn--ghost btn--sq btn-share" type="button" aria-label="Copy invite link for ${esc(d.name)}"><span class="mi">share</span></button>
        ${menu}
      </div>`;

    // own devices join with the account token — no PIN needed
    if (d.online) card.querySelector(".btn-connect").onclick = () => (d.mine ? startSessionToken(d) : askPin(d));
    const wakeBtn = card.querySelector(".btn-wake");
    if (wakeBtn) wakeBtn.onclick = () => wakeDevice(d, wakeBtn);
    card.querySelector(".btn-share").onclick = () => shareDevice(d);
    const rm = card.querySelector(".btn-remove");
    if (rm) rm.onclick = () => removeDevice(d, rm);
    const lg = card.querySelector(".btn-logs");
    if (lg) lg.onclick = () => fetchLogs(d);
    if (openId && d.id === openId) { const m = card.querySelector(".dev-more"); if (m) m.setAttribute("open", ""); }

    list.appendChild(card);
  });
}
async function removeDevice(d, btn) {
  if (!confirm(`Remove "${d.name || d.id}" from your devices? You can re-add it anytime with its link code.`)) return;
  btn.disabled = true;
  try {
    const token = sbOn() ? await sbToken() : null;
    const r = await fetch(serverBase() + "/api/devices/" + encodeURIComponent(d.id), {
      method: "DELETE",
      headers: token ? { Authorization: "Bearer " + token } : {},
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || "remove failed");
    toast("Device removed");
    loadDevices(true);
  } catch (e) { toast(e.message); btn.disabled = false; }
}
async function wakeDevice(d, btn) {
  btn.disabled = true;
  const orig = btn.innerHTML;
  btn.innerHTML = '<span class="mi spin-ico">progress_activity</span><span>Waking…</span>';
  try {
    const token = await sbToken();
    const r = await fetch(serverBase() + "/api/wake", {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + token },
      body: JSON.stringify({ id: d.id }),
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || "wake failed");
    toast("Wake signal sent — the PC should come online shortly");
    // poll for the agent coming online
    let tries = 0;
    const iv = setInterval(async () => {
      tries++;
      await loadDevices(true);
      const dev = lastDevices.find((x) => x.id === d.id);
      if ((dev && dev.online) || tries > 10) {
        clearInterval(iv);
        if (dev && dev.online) toast("PC is online");
      }
    }, 3000);
  } catch (e) {
    toast(e.message);
    btn.disabled = false;
    btn.innerHTML = orig;
  }
}
// v8.1: fetch agent.log from the PC (same-account only)
let logDevice = null;
async function fetchLogs(d) {
  logDevice = d;
  $("log-modal").classList.remove("hidden");
  $("log-content").textContent = "Requesting logs from PC…";
  // join a viewer session to receive the logs. The get-logs request must
  // wait for the join to complete — sending it before the socket opened
  // silently lost it (v9.0 fix via joinHooks).
  try {
    if (sessionLive && deviceId === d.id && ws && ws.readyState === 1) callSend({ t: "get-logs" });
    else { joinHooks.push(() => callSend({ t: "get-logs" })); await startSessionToken(d); }
  } catch (e) {
    $("log-content").textContent = "Failed to connect: " + e.message;
  }
}
function onLogsMessage(m) {
  if (m.t !== "logs") return false;
  if (m.error) $("log-content").textContent = m.error;
  else $("log-content").textContent = m.data || "(empty log)";
  return true;
}
$("log-close").onclick = () => { $("log-modal").classList.add("hidden"); logDevice = null; };
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
const brm = $("btn-refresh-m");
if (brm) brm.onclick = () => { brm.classList.remove("spin"); void brm.offsetWidth; brm.classList.add("spin"); loadDevices(); };
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
// v9.0 (Gen-Z edition): the old connect flow opened one WebSocket and gave
// up on the first error — fatal on Render's free tier, where the relay
// sleeps and needs ~50s to wake. Now: creds are kept in memory for the
// session, a failed open triggers a health-ping wake-up + retry, and a
// mid-session drop auto-reconnects with backoff instead of dying.
let sessDevice = null, sessCreds = null, sessIntentionalClose = false;
let wsAttempt = 0, wsReconnectTimer = null, joinHooks = [], sessionLive = false;

function startSessionWith(d, creds) {
  sessDevice = d; sessCreds = creds;
  sessIntentionalClose = false; wsAttempt = 0; sessionLive = false;
  deviceId = d.id; deviceName = d.name;
  $("sess-name").textContent = d.name;
  show("scr-session");
  bindCanvasInput();
  ptrs.clear(); gesture = null; gestureActive = false;
  resetZoom();
  openSessionWs();
}

async function wakeServer() {
  // Poll /api/health until the relay answers (free-tier cold start).
  for (let i = 0; i < 8; i++) {
    try {
      const ctl = new AbortController();
      const to = setTimeout(() => ctl.abort(), 12000);
      const r = await fetch(serverBase() + "/api/health", { signal: ctl.signal, cache: "no-store" });
      clearTimeout(to);
      if (r.ok) return true;
    } catch {}
    if (!sessIntentionalClose) setStatus("Waking the server — free servers nap, up to a minute 😴");
    await new Promise((res) => setTimeout(res, 4000));
  }
  return false;
}

function openSessionWs() {
  closeWs();
  if (sessIntentionalClose || !sessDevice) return;
  setStatus(wsAttempt ? "Reconnecting…" : "Connecting…");
  ws = new WebSocket(wsUrl());
  ws.binaryType = "arraybuffer";
  let opened = false;
  ws.onopen = () => {
    opened = true;
    ws.send(JSON.stringify({ t: "join", id: sessDevice.id, ...sessCreds,
      name: myProfile.display_name || (sbSignedIn() ? sbSession.email.split("@")[0] : "Guest"),
      avatar: myProfile.avatar_url || "" }));
  };
  ws.onmessage = onWsMsg;
  ws.onclose = () => {
    if (sessIntentionalClose || !sessDevice) return;
    if (!opened && wsAttempt < 3) {
      // socket never opened — the relay is almost certainly asleep
      wsAttempt++;
      setStatus("Waking the server…");
      wakeServer().then(() => { if (!sessIntentionalClose) openSessionWs(); });
      return;
    }
    if (wsAttempt < 6) {
      const delay = [1000, 2000, 4000, 8000, 12000][wsAttempt] || 12000;
      wsAttempt++;
      setStatus("Connection dropped — reconnecting…", true);
      clearTimeout(wsReconnectTimer);
      wsReconnectTimer = setTimeout(() => { if (!sessIntentionalClose) openSessionWs(); }, delay);
    } else {
      setStatus("Disconnected. Tap ✕ and try again.", true);
    }
  };
  ws.onerror = () => { try { ws.close(); } catch {} };
}
function closeWs() {
  if (ws) { ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null; try { ws.close(); } catch {} }
  ws = null;
}
$("btn-disconnect").onclick = () => {
  sessIntentionalClose = true;
  clearTimeout(wsReconnectTimer);
  sessCreds = null; sessDevice = null;
  closeWs(); enterDevices();
};
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
    const wasReconnect = sessionLive || wsAttempt > 0;
    wsAttempt = 0; sessionLive = true;
    setStatus(""); $("sess-dot").className = "dot on";
    if (m.screen) sizeCanvas(m.screen.w, m.screen.h);
    ws.send(JSON.stringify({ t: "keyframe" })); // ask for a full frame now
    if (wasReconnect && frameW) toast("Reconnected ⚡");
    const hooks = joinHooks; joinHooks = [];
    hooks.forEach((fn) => { try { fn(); } catch {} });
  }
  else if (m.t === "error") setStatus("Error: " + (m.msg || "unknown"), true);
  else if (m.t === "agent_gone") { setStatus("Device went offline.", true); $("sess-dot").className = "dot off"; }
  else if (m.t === "agent_back") { setStatus(""); $("sess-dot").className = "dot on"; }
  else if (m.t === "chat") addChat(m.text, false);
  else if (m.t === "cursor") moveRemoteCursor(m.x, m.y);
  else if (m.t === "file_meta") noteFileFromAgent(m);
  else if (m.t === "call-accept" || m.t === "call-end" || m.t === "call-decline" || m.t === "av-state") onCallSignal(m);
  else if (m.t === "logs") onLogsMessage(m);
}

function handleBinary(u8) {
  const kind = u8[0];
  if (kind === BIN_FRAME) { drawFrame(u8.subarray(1)); return; }
  if (kind === BIN_TILE) { queueTile(u8); return; }
  if (kind === BIN_FILE) { recvFileChunk(u8.subarray(1)); return; }
  if (kind === 0x06) { showPCVideo(u8.subarray(1)); return; } // PC camera frame
  if (kind === 0x07) { playPCAudio(u8.subarray(1)); return; } // PC mic PCM
}

// (meeting showPCVideo is defined in the meeting section below)
// PC mic PCM (8kHz mono 16-bit) -> speakers
let pcAudioCtx = null;
function playPCAudio(pcm) {
  try {
    pcAudioCtx = pcAudioCtx || new (window.AudioContext || window.webkitAudioContext)();
    if (pcAudioCtx.state === "suspended") pcAudioCtx.resume(); // autoplay policy can leave it parked
    const n = pcm.length / 2;
    const buf = pcAudioCtx.createBuffer(1, n, 8000);
    const ch = buf.getChannelData(0);
    const dv = new DataView(pcm.buffer, pcm.byteOffset, pcm.byteLength);
    for (let i = 0; i < n; i++) ch[i] = dv.getInt16(i * 2, true) / 32768;
    const src = pcAudioCtx.createBufferSource();
    src.buffer = buf; src.connect(pcAudioCtx.destination); src.start();
  } catch {}
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

// Full frames: latest-wins. While a decode is in flight, only the newest
// waiting frame is kept — older ones are worthless and just add latency.
let pendingFrame = null;
async function drawFrame(jpeg) {
  if (decoding) { pendingFrame = jpeg; return; }
  decoding = true;
  try {
    let cur = jpeg;
    while (cur) {
      const bmp = await createImageBitmap(new Blob([cur], { type: "image/jpeg" }));
      if (canvas.width !== bmp.width || canvas.height !== bmp.height) sizeCanvas(bmp.width, bmp.height);
      ctx.drawImage(bmp, 0, 0);
      bmp.close();
      framesThisSec++;
      cur = pendingFrame; pendingFrame = null;
    }
    fitCanvas(); // re-fit if the layout settled after the first frames
    if ($("sess-status").textContent) setStatus("");
  } catch {} finally { decoding = false; }
}

// auto status check: refresh the device list every 15s while on the devices page
setInterval(() => {
  if (document.hidden) return; // don't burn battery/data in a background tab
  if ($("page-devices") && !$("page-devices").classList.contains("hidden") && !$("scr-session").classList.contains("active")) loadDevices(true);
}, 15000);

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

// ---- tiled rendering: tiles are composited straight onto the canvas.
// Tiles arrive as: 0x03 + uint16BE idx + JPEG. idx = row*cols + col.
// v9.0 zero-lag pass: the old pipeline chained every tile through one
// promise queue — when tiles arrived faster than they decoded, latency
// snowballed. Now tiles coalesce in a Map: only the NEWEST tile per cell
// is kept, stale ones are dropped, and a backlog flood resets to a fresh
// keyframe instead of seconds of catch-up.
const tilePending = new Map();
let tileDraining = false;
function queueTile(u8) {
  if (!frameW) return; // no keyframe yet — will come shortly
  const idx = (u8[1] << 8) | u8[2];
  tilePending.set(idx, u8);
  if (tilePending.size > 400) {
    tilePending.clear();
    if (ws && ws.readyState === 1) ws.send(JSON.stringify({ t: "keyframe" }));
    return;
  }
  if (!tileDraining) { tileDraining = true; requestAnimationFrame(drainTiles); }
}
async function drainTiles() {
  try {
    while (tilePending.size) {
      const batch = Array.from(tilePending.values());
      tilePending.clear();
      for (const t of batch) await drawTile(t);
    }
  } catch {} finally { tileDraining = false; }
}
async function drawTile(u8) {
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


/* ---------- meeting (v8.8, from scratch) ----------
   Full meeting mode: PC screen stays the main stage, the PC camera appears
   as a corner PiP, your camera as a second tile, meeting controls below. */
let callState = "idle"; // idle | calling | incall
let callRingTimer = null;
let callStream = null, callMicOn = true, callCamOn = true;
let callFrameTimer = null, callAudioCtx = null, callAudioNode = null;
let meetingTimer = null, meetingSecs = 0;
const isOwnerViewer = () => sbOn() && sbSignedIn();

function meetingEnter() {
  $("meeting-pips").classList.remove("hidden");
  $("session-toolbar").classList.add("hidden");
  $("meeting-bar").classList.remove("hidden");
  $("btn-call").classList.add("on");
  // meeting timer pill in the session header
  let pill = $("meeting-timer");
  if (!pill) {
    pill = document.createElement("span");
    pill.id = "meeting-timer";
    $("sess-name").after(pill);
  }
  pill.textContent = "Meeting 00:00";
  meetingSecs = 0;
  clearInterval(meetingTimer);
  meetingTimer = setInterval(() => {
    meetingSecs++;
    const m = String(Math.floor(meetingSecs / 60)).padStart(2, "0");
    const s = String(meetingSecs % 60).padStart(2, "0");
    const el = $("meeting-timer");
    if (el) el.textContent = "Meeting " + m + ":" + s;
  }, 1000);
  // PC controls only for the owner
  const owner = isOwnerViewer();
  $("m-pc-mic").style.display = owner ? "" : "none";
  $("m-pc-cam").style.display = owner ? "" : "none";
  updatePCMeetingUI();
}
function meetingExit() {
  $("meeting-pips").classList.add("hidden");
  $("session-toolbar").classList.remove("hidden");
  $("meeting-bar").classList.add("hidden");
  $("btn-call").classList.remove("on");
  clearInterval(meetingTimer); meetingTimer = null;
  const pill = $("meeting-timer");
  if (pill) pill.remove();
}

$("btn-call").onclick = () => {
  if (callState === "idle") meetingStart();
};
function callSend(o) { if (ws && ws.readyState === 1) ws.send(JSON.stringify(o)); }

async function meetingStart() {
  if (callState !== "idle") return;
  try {
    callStream = await navigator.mediaDevices.getUserMedia({ video: { width: 320, height: 240 }, audio: true });
  } catch (e) { toast("Camera/mic blocked — allow access to join the meeting."); return; }
  $("meeting-local-video").srcObject = callStream;
  callState = "calling";
  meetingEnter();
  toast("Ringing the PC…");
  callSend({ t: "call-start", name: myProfile.display_name || "Guest" });
  // if the PC doesn't answer in 20s, tell the user why (old agent? offline?)
  clearTimeout(callRingTimer);
  callRingTimer = setTimeout(() => {
    if (callState === "calling") {
      meetingStop();
      toast("No answer — is the v7.0+ agent running on the PC?");
    }
  }, 20000);
  // auto-start streaming; agent answers with call-accept
  startCallMedia();
}
function startCallMedia() {
  // camera frames: JPEG snapshots ~5fps as binary 0x04
  const vid = $("meeting-local-video"), cv = document.createElement("canvas");
  cv.width = 320; cv.height = 240;
  const cx = cv.getContext("2d");
  callFrameTimer = setInterval(() => {
    if (callState !== "incall" || !callCamOn || !ws || ws.readyState !== 1) return;
    try {
      cx.drawImage(vid, 0, 0, 320, 240);
      cv.toBlob((b) => {
        if (!b) return;
        b.arrayBuffer().then((ab) => {
          const out = new Uint8Array(1 + ab.byteLength);
          out[0] = 0x04; out.set(new Uint8Array(ab), 1);
          ws.send(out);
        });
      }, "image/jpeg", 0.6);
    } catch {}
  }, 200);
  // mic: downsample to 8kHz mono PCM as binary 0x05
  try {
    callAudioCtx = new (window.AudioContext || window.webkitAudioContext)();
    if (callAudioCtx.state === "suspended") callAudioCtx.resume();
    const src = callAudioCtx.createMediaStreamSource(callStream);
    const len = 4096;
    callAudioNode = callAudioCtx.createScriptProcessor(len, 1, 1);
    const inRate = callAudioCtx.sampleRate, outRate = 8000;
    callAudioNode.onaudioprocess = (e) => {
      if (callState !== "incall" || !callMicOn || !ws || ws.readyState !== 1) return;
      const inp = e.inputBuffer.getChannelData(0);
      const step = inRate / outRate, n = Math.floor(inp.length / step);
      const pcm = new Int16Array(n);
      for (let i = 0; i < n; i++) {
        const v = Math.max(-1, Math.min(1, inp[Math.floor(i * step)]));
        pcm[i] = v < 0 ? v * 0x8000 : v * 0x7fff;
      }
      const out = new Uint8Array(1 + pcm.byteLength);
      out[0] = 0x05; out.set(new Uint8Array(pcm.buffer), 1);
      ws.send(out);
    };
    src.connect(callAudioNode); callAudioNode.connect(callAudioCtx.destination);
  } catch {}
  callSend({ t: "av-state", cam: callCamOn, mic: callMicOn });
}
function meetingStop() {
  callState = "idle";
  clearTimeout(callRingTimer);
  clearInterval(callFrameTimer); callFrameTimer = null;
  if (callAudioNode) { try { callAudioNode.disconnect(); } catch {} callAudioNode = null; }
  if (callAudioCtx) { try { callAudioCtx.close(); } catch {} callAudioCtx = null; }
  if (callStream) { callStream.getTracks().forEach((t) => t.stop()); callStream = null; }
  $("meeting-local-video").srcObject = null;
  $("meeting-pc-video").classList.add("hidden");
  const demo = $("meeting-pc-demo");
  if (demo) demo.classList.remove("hidden");
  $("meeting-pc-off").style.display = "none";
  meetingExit();
}
$("m-end").onclick = () => { callSend({ t: "call-end" }); meetingStop(); toast("Meeting ended"); };
$("m-mic").onclick = () => {
  callMicOn = !callMicOn;
  const b = $("m-mic");
  b.classList.toggle("off", !callMicOn);
  b.querySelector(".t-label").textContent = callMicOn ? "Mic" : "Muted";
  $("meeting-local-mute").classList.toggle("hidden", callMicOn);
  callSend({ t: "av-state", cam: callCamOn, mic: callMicOn });
};
$("m-cam").onclick = () => {
  callCamOn = !callCamOn;
  const b = $("m-cam");
  b.classList.toggle("off", !callCamOn);
  b.querySelector(".t-label").textContent = callCamOn ? "Camera" : "Off";
  $("meeting-local-off").classList.toggle("hidden", callCamOn);
  $("meeting-local-video").style.visibility = callCamOn ? "visible" : "hidden";
  callSend({ t: "av-state", cam: callCamOn, mic: callMicOn });
};
$("m-chat").onclick = () => toggleDrawer("chat-panel");
$("m-pc-mic").onclick = () => {
  // owner viewer toggles the PC's mic
  pcMicOn = !pcMicOn;
  callSend({ t: "av-ctrl", target: "agent", mic: pcMicOn });
  updatePCMeetingUI();
};
$("m-pc-cam").onclick = () => {
  // owner viewer toggles the PC's camera
  pcCamOn = !pcCamOn;
  callSend({ t: "av-ctrl", target: "agent", cam: pcCamOn });
  updatePCMeetingUI();
};
let pcCamOn = true, pcMicOn = true;
function onCallSignal(m) {
  if (m.t === "call-accept" && callState === "calling") { callState = "incall"; toast("Meeting connected"); }
  else if (m.t === "call-end" && callState !== "idle") { meetingStop(); toast("They ended the meeting"); }
  else if (m.t === "call-decline" && callState === "calling") { meetingStop(); toast("They declined"); }
  else if (m.t === "av-state" && m.side === "agent") {
    pcCamOn = m.cam !== false; pcMicOn = m.mic !== false;
    updatePCMeetingUI();
  }
}
function updatePCMeetingUI() {
  const off = $("meeting-pc-off"), img = $("meeting-pc-video"), demo = $("meeting-pc-demo");
  if (!off || !img) return;
  if (pcCamOn) {
    // silent demo fallback: show the demo picture until real frames arrive
    if (demo) demo.classList.remove("hidden");
    off.style.display = "none";
    $("meeting-pc-msg").textContent = "PC camera…";
  } else {
    img.classList.add("hidden");
    if (demo) demo.classList.remove("hidden");
    off.style.display = "none";
  }
  const micBtn = $("m-pc-mic"), camBtn = $("m-pc-cam");
  micBtn.classList.toggle("off", !pcMicOn);
  micBtn.querySelector(".t-label").textContent = pcMicOn ? "PC mic" : "PC muted";
  camBtn.classList.toggle("off", !pcCamOn);
}
// PC camera JPEG frame -> corner PiP (hides the demo silently)
let pcVideoURL = null;
function showPCVideo(jpeg) {
  const v = $("meeting-pc-video");
  if (!v) return;
  try {
    if (pcVideoURL) URL.revokeObjectURL(pcVideoURL);
    pcVideoURL = URL.createObjectURL(new Blob([jpeg], { type: "image/jpeg" }));
    v.src = pcVideoURL;
    v.classList.remove("hidden");
    const demo = $("meeting-pc-demo");
    if (demo) demo.classList.add("hidden");
    $("meeting-pc-off").style.display = "none";
  } catch {}
}
// meeting PiPs are draggable
(function() {
  for (const id of ["meeting-pip-pc", "meeting-pip-local"]) {
    const pip = $(id);
    if (!pip) continue;
    let sx, sy, ox, oy, dragging = false;
    pip.style.touchAction = "none";
    pip.addEventListener("pointerdown", (e) => {
      dragging = true; sx = e.clientX; sy = e.clientY;
      const r = pip.getBoundingClientRect(), pr = pip.parentElement.getBoundingClientRect();
      ox = r.left - pr.left; oy = r.top - pr.top;
      pip.setPointerCapture(e.pointerId);
    });
    pip.addEventListener("pointermove", (e) => {
      if (!dragging) return;
      const pr = pip.parentElement.getBoundingClientRect();
      let nx = ox + e.clientX - sx, ny = oy + e.clientY - sy;
      nx = Math.max(0, Math.min(nx, pr.width - pip.offsetWidth));
      ny = Math.max(0, Math.min(ny, pr.height - pip.offsetHeight));
      pip.style.left = nx + "px"; pip.style.top = ny + "px";
      pip.style.right = "auto"; pip.style.bottom = "auto";
    });
    pip.addEventListener("pointerup", () => dragging = false);
  }
})();
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

/* ---------- clear cache & reload latest ---------- */
$("btn-clear-cache").onclick = async () => {
  const btn = $("btn-clear-cache");
  btn.disabled = true; btn.textContent = "Clearing…";
  try {
    if ("serviceWorker" in navigator) {
      const regs = await navigator.serviceWorker.getRegistrations();
      await Promise.all(regs.map((r) => r.unregister()));
    }
    if ("caches" in window) {
      const keys = await caches.keys();
      await Promise.all(keys.map((k) => caches.delete(k)));
    }
  } catch (e) {}
  location.reload(true);
};
/* ---------- PWA ---------- */
if ("serviceWorker" in navigator) navigator.serviceWorker.register("sw.js").catch(() => {});
let deferredInstall = null;
window.addEventListener("beforeinstallprompt", (e) => { e.preventDefault(); deferredInstall = e; });
bindGestures();
boot();
