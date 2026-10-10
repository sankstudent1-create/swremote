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
  document.querySelectorAll("#side-nav button").forEach((b) => {
    const on = b.dataset.page === page;
    b.className = "flex items-center gap-3 px-4 py-2.5 rounded-xl font-medium text-sm transition-colors " +
      (on ? "bg-primary-container text-on-primary-container" : "text-on-surface-variant hover:bg-surface-container");
  });
  document.querySelectorAll("#mobile-nav button").forEach((b) => {
    const on = b.dataset.page === page;
    b.className = "p-2.5 rounded-xl " + (on ? "text-primary bg-primary-fixed/40" : "text-on-surface-variant");
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
    const j = await sbCall("/rest/v1/profiles?select=display_name,avatar_url", null, { Authorization: "Bearer " + token }, "GET");
    if (Array.isArray(j) && j[0]) myProfile = { display_name: j[0].display_name || "", avatar_url: j[0].avatar_url || "" };
  } catch {}
}
function avatarHTML(size, cls) {
  if (myProfile.avatar_url) return `<img src="${esc(myProfile.avatar_url)}" class="${cls} rounded-full object-cover shrink-0" style="width:${size}px;height:${size}px" alt="">`;
  const ch = (myProfile.display_name || sbSession?.email || "?")[0].toUpperCase();
  return `<div class="${cls} rounded-full bg-primary text-on-primary grid place-items-center font-bold shrink-0" style="width:${size}px;height:${size}px">${esc(ch)}</div>`;
}
async function uploadAvatar(file) {
  const token = await sbToken();
  const uid = sbUserId();
  if (!token || !uid) throw new Error("Sign in required");
  const ext = (file.name.split(".").pop() || "jpg").toLowerCase().replace(/[^a-z]/g, "") || "jpg";
  const path = `${uid}/avatar.${ext}`;
  let up;
  try {
    up = await fetch(sbUrl() + "/storage/v1/object/avatars/" + path, {
      method: "POST", headers: { Authorization: "Bearer " + token, "Content-Type": file.type || "image/jpeg", "x-upsert": "true" },
      body: file,
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
    <div class="flex flex-col items-center text-center mb-6">
      <div class="relative mb-4">
        ${avatarHTML(88, "text-4xl")}
        ${signed ? `<label class="absolute -bottom-1 -right-1 w-9 h-9 rounded-full bg-primary text-on-primary grid place-items-center cursor-pointer shadow-lg hover:scale-105 transition-transform" title="Change picture">
          <span class="material-symbols-outlined text-lg">photo_camera</span>
          <input type="file" id="avatar-file" accept="image/*" class="hidden">
        </label>` : ""}
      </div>
      ${signed ? `<input id="acct-name" class="auth-input font-display font-bold text-2xl text-center mb-1" value="${esc(dname)}" placeholder="Your name">`
               : `<div class="font-display font-bold text-2xl mb-1">Guest</div>`}
      <div class="text-sm text-on-surface-variant">${signed ? esc(sbSession.email) : "Browsing without an account"}</div>
      ${signed && myProfile.avatar_url ? `<div class="mt-2 inline-flex items-center gap-1.5 px-3 py-1 rounded-full bg-tertiary-fixed text-on-tertiary-fixed-variant text-xs font-semibold"><span class="material-symbols-outlined text-sm">verified</span>Profile complete</div>` : ""}
    </div>
    ${signed ? `<button id="btn-save-profile" class="w-full py-3.5 rounded-2xl bg-primary hover:bg-primary-container text-on-primary font-semibold transition-all hover:-translate-y-0.5 shadow-sm mb-3">Save profile</button>` : ""}
    ${signed
      ? `<button id="btn-signout2" class="w-full py-3 rounded-2xl bg-surface-container-low hover:bg-surface-container font-semibold transition-colors">Sign out</button>`
      : `<button id="btn-signin2" class="w-full py-3.5 rounded-2xl bg-primary hover:bg-primary-container text-on-primary font-semibold transition-all hover:-translate-y-0.5 shadow-sm">Sign in / create account</button>`}
    <div class="mt-6 pt-6 border-t border-surface-variant">
      <div class="text-[11px] font-semibold text-outline uppercase tracking-wider mb-2">Relay server</div>
      <div class="flex gap-2">
        <input id="acct-server" class="auth-input flex-1" value="${esc(serverBase())}">
        <button id="btn-acct-server" class="px-5 rounded-xl bg-surface-container-low hover:bg-surface-container font-semibold whitespace-nowrap transition-colors">Save</button>
      </div>
    </div>
    <button id="btn-install2" class="hidden w-full mt-4 py-3 rounded-2xl bg-surface-container-low hover:bg-surface-container font-semibold transition-colors">Install SWRemote app</button>
    <p class="text-xs text-outline mt-6 text-center">SWRemote · calm horizon edition</p>`;
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
    $("avatar-file").onchange = (e) => {
      const f = e.target.files[0];
      if (f) uploadAvatar(f).catch((err) => toast(err.message || "Could not upload picture"));
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
    av.innerHTML = `<img src="${esc(myProfile.avatar_url)}" class="w-9 h-9 rounded-full object-cover" alt="">`;
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
    ? `Signed in as <b>${esc(sbSession.email)}</b> — manage everything from the <b>Account</b> tab, or <a href="#" id="link-signout" class="text-primary font-semibold">sign out</a>.`
    : `You're browsing as a guest — <a href="#" id="link-signin" class="text-primary font-semibold">sign in</a> to see your devices.`;
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
  if (!quiet) list.innerHTML = '<div class="bg-surface-container-lowest rounded-2xl p-5 shadow-sm animate-pulse"><div class="h-4 bg-surface-container rounded w-2/3 mb-3"></div><div class="h-4 bg-surface-container rounded w-1/3"></div></div>'
    + '<div class="bg-surface-container-lowest rounded-2xl p-5 shadow-sm animate-pulse"><div class="h-4 bg-surface-container rounded w-2/3 mb-3"></div><div class="h-4 bg-surface-container rounded w-1/3"></div></div>';
  try {
    const headers = {};
    const token = sbOn() ? await sbToken() : null;
    if (token) headers.Authorization = "Bearer " + token;
    const r = await fetch(serverBase() + "/api/devices", { headers });
    const j = await r.json();
    lastDevices = j.devices || [];
    renderDeviceList(lastDevices);
    const srt = $("side-relay-text");
    if (srt) srt.textContent = "● Connected";
    // direct link support: ?id=123456789 opens the PIN box for that device (fresh loads only)
    const want = new URLSearchParams(location.search).get("id");
    if (want) {
      const target = lastDevices.find((d) => d.id === want.trim());
      if (target && target.online) (target.mine ? startSessionToken(target) : askPin(target));
      else if (target) toast("That PC is offline right now.");
    }
  } catch (e) {
    $("dev-count").textContent = "";
    list.innerHTML = `<div class="col-span-full bg-surface-container-lowest rounded-2xl p-10 shadow-sm flex flex-col items-center text-center">
      <span class="material-symbols-outlined text-5xl text-outline mb-3">cloud_off</span>
      <div class="font-display font-bold text-xl mb-1">Cannot reach the server.</div>
      <div class="text-sm text-on-surface-variant">${esc(e.message)}<br>Check your connection and tap refresh.</div></div>`;
  }
}
function renderDeviceList(devices) {
  const list = $("device-list");
  const online = devices.filter((d) => d.online).length;
  $("dev-count").textContent = devices.length
    ? `${online} of ${devices.length} online` : "";
  if (!devices.length) {
    const emptyArt = `<div class="w-24 h-24 rounded-3xl bg-gradient-to-br from-primary-fixed to-tertiary-fixed grid place-items-center mb-4 shadow-lg">
           <span class="material-symbols-outlined text-5xl text-primary">computer</span>
         </div>`;
    list.innerHTML = sbOn() && sbSignedIn()
      ? `<div class="col-span-full bg-surface-container-lowest rounded-3xl p-10 shadow-sm flex flex-col items-center text-center">
           ${emptyArt}
           <div class="font-display font-bold text-2xl mb-2">No devices yet.</div>
           <div class="text-sm text-on-surface-variant max-w-sm mb-5">Your PCs will appear here once you link them. It takes less than a minute.</div>
           <div class="flex flex-col gap-2 text-left w-full max-w-sm">
             <div class="flex items-center gap-3 p-3 rounded-xl bg-surface-container-low"><span class="w-8 h-8 rounded-full bg-primary text-on-primary grid place-items-center font-bold shrink-0">1</span><span class="text-sm">Run <b>SWRemote-Setup.exe</b> on your Windows PC</span></div>
             <div class="flex items-center gap-3 p-3 rounded-xl bg-surface-container-low"><span class="w-8 h-8 rounded-full bg-primary text-on-primary grid place-items-center font-bold shrink-0">2</span><span class="text-sm">Tap <b>Add device</b> above and enter the link code</span></div>
             <div class="flex items-center gap-3 p-3 rounded-xl bg-surface-container-low"><span class="w-8 h-8 rounded-full bg-primary text-on-primary grid place-items-center font-bold shrink-0">3</span><span class="text-sm">Click your PC to take control from anywhere</span></div>
           </div>
         </div>`
      : `<div class="col-span-full bg-surface-container-lowest rounded-3xl p-10 shadow-sm flex flex-col items-center text-center">
           ${emptyArt}
           <div class="font-display font-bold text-2xl mb-2">No devices yet.</div>
           <div class="text-sm text-on-surface-variant max-w-sm">Open <b>SWRemote-Agent.exe</b> on a Windows PC first, then sign in to link it.</div>
         </div>`;
    return;
  }
  list.innerHTML = "";
  const q = ($("dev-search").value || "").trim().toLowerCase();
  const shown = q ? devices.filter((d) => (d.name + " " + d.id).toLowerCase().includes(q)) : devices;
  if (!shown.length) {
    list.innerHTML = `<div class="col-span-full bg-surface-container-lowest rounded-2xl p-10 shadow-sm flex flex-col items-center text-center">
      <span class="material-symbols-outlined text-5xl text-outline mb-3">search_off</span>
      <div class="font-display font-bold text-xl mb-1">No matches.</div>
      <div class="text-sm text-on-surface-variant">Try a different search.</div></div>`;
    return;
  }
    shown.forEach((d) => {
      const card = document.createElement("div");
      card.className = "device-card bg-surface-container-lowest rounded-2xl p-5 shadow-sm hover:shadow-md transition-all duration-200 flex flex-col justify-between group" + (d.online ? "" : " opacity-90");
      card.innerHTML = `
        <div class="flex flex-col gap-4">
          <div class="flex items-start justify-between gap-3">
            <div class="flex items-center gap-3 min-w-0">
              <div class="w-11 h-11 rounded-xl ${d.online ? "bg-secondary-container text-primary" : "bg-surface-container text-on-surface-variant"} flex items-center justify-center shrink-0 shadow-sm">
                <span class="material-symbols-outlined text-2xl">desktop_windows</span>
              </div>
              <div class="flex flex-col min-w-0">
                <span class="font-semibold text-on-surface truncate group-hover:text-primary transition-colors">${esc(d.name)}</span>
                <span class="text-[11px] text-on-surface-variant font-mono mt-0.5">#${esc(d.id)}</span>
              </div>
            </div>
            <span class="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full ${d.online ? "bg-tertiary-fixed text-on-tertiary-fixed-variant" : d.serviceOnline ? "bg-secondary-fixed text-primary" : "bg-surface-container-high text-on-surface-variant"} text-[11px] font-semibold shrink-0">
              <span class="w-1.5 h-1.5 rounded-full ${d.online ? "bg-tertiary animate-pulse" : d.serviceOnline ? "bg-primary animate-pulse" : "bg-surface-variant"}"></span>${d.online ? "Online" : d.serviceOnline ? "Service on" : "Offline"}
            </span>
          </div>
          <div class="bg-surface-container-low rounded-xl px-3 py-2.5 flex items-center justify-between">
            <span class="text-sm ${d.online ? "text-on-surface font-medium" : "text-on-surface-variant"}">${d.online ? "Active now" : "seen " + relTime(d.lastSeen)}</span>
          </div>
        </div>
        <div class="flex items-center gap-2 pt-5">
          <button class="btn-share flex-1 flex items-center justify-center gap-1.5 py-2 px-3 rounded-xl bg-surface-container-low hover:bg-surface-container text-on-surface text-sm font-medium transition-colors"><span class="material-symbols-outlined text-base text-secondary">share</span><span>Share</span></button>
          ${!d.online && d.serviceOnline && d.mine
            ? `<button class="btn-wake flex-[1.4] flex items-center justify-center gap-1.5 py-2 px-3 rounded-xl bg-tertiary-fixed text-on-tertiary-fixed-variant text-sm font-medium shadow-sm transition-all"><span class="material-symbols-outlined text-base">power_settings_new</span><span>Wake</span></button>`
            : `<button class="btn-connect flex-[1.4] flex items-center justify-center gap-1.5 py-2 px-3 rounded-xl ${d.online ? "bg-primary hover:bg-primary-container text-on-primary" : "bg-surface-container-high text-on-surface-variant"} text-sm font-medium shadow-sm transition-all" ${d.online ? "" : "disabled"}><span class="material-symbols-outlined text-base">${d.mine ? "play_arrow" : "lock"}</span><span>${d.mine ? "Open" : "Connect"}</span></button>`}
        </div>`;
      // own devices join with the account token — no PIN needed
      if (d.online) card.querySelector(".btn-connect").onclick = () => d.mine ? startSessionToken(d) : askPin(d);
      const wakeBtn = card.querySelector(".btn-wake");
      if (wakeBtn) wakeBtn.onclick = () => wakeDevice(d, wakeBtn);
      card.querySelector(".btn-share").onclick = () => shareDevice(d);
      // always offer Remove on own devices
      if (d.mine) {
        const delBtn = document.createElement("button");
        delBtn.className = "mt-2 w-full flex items-center justify-center gap-1.5 py-2 px-3 rounded-xl bg-surface-container-low hover:bg-error-container text-on-surface hover:text-on-error-container text-xs font-medium transition-colors";
        delBtn.innerHTML = '<span class="material-symbols-outlined text-base">delete</span><span>Remove device</span>';
        delBtn.onclick = () => removeDevice(d, delBtn);
        card.appendChild(delBtn);
      }
      // v8.1: logs button for own devices
      if (d.mine && d.online) {
        const logBtn = document.createElement("button");
        logBtn.className = "mt-2 w-full flex items-center justify-center gap-1.5 py-2 px-3 rounded-xl bg-surface-container-low hover:bg-surface-container text-on-surface text-xs font-medium transition-colors";
        logBtn.innerHTML = '<span class="material-symbols-outlined text-base">description</span><span>View PC logs</span>';
        logBtn.onclick = () => fetchLogs(d);
        card.appendChild(logBtn);
      }
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
  btn.innerHTML = '<span class="material-symbols-outlined text-base animate-spin">refresh</span><span>Waking…</span>';
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
  // join a viewer session to receive the logs
  try {
    await startSessionToken(d);
    callSend({ t: "get-logs" });
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
function startSessionWith(d, creds) {
  deviceId = d.id; deviceName = d.name;
  $("sess-name").textContent = d.name;
  show("scr-session");
  setStatus("Connecting…");
  closeWs();
  ws = new WebSocket(wsUrl());
  ws.binaryType = "arraybuffer";
  ws.onopen = () => ws.send(JSON.stringify({ t: "join", id: deviceId, ...creds,
    name: myProfile.display_name || (sbSignedIn() ? sbSession.email.split("@")[0] : "Guest"),
    avatar: myProfile.avatar_url || "" }));
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

// PC camera frame -> remote video panel
let pcVideoURL = null;
function showPCVideo(jpeg) {
  const v = $("call-remote-video");
  if (!v) return;
  try {
    if (pcVideoURL) URL.revokeObjectURL(pcVideoURL);
    pcVideoURL = URL.createObjectURL(new Blob([jpeg], { type: "image/jpeg" }));
    v.src = pcVideoURL;
    v.classList.remove("hidden");
  } catch {}
}
// PC mic PCM (8kHz mono 16-bit) -> speakers
let pcAudioCtx = null;
function playPCAudio(pcm) {
  try {
    pcAudioCtx = pcAudioCtx || new (window.AudioContext || window.webkitAudioContext)();
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

// auto status check: refresh the device list every 15s while on the devices page
setInterval(() => {
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


/* ---------- video call (v6.0) ---------- */
let callState = "idle"; // idle | calling | incall
let callRingTimer = null;
let callStream = null, callMicOn = true, callCamOn = true;
let callFrameTimer = null, callAudioCtx = null, callAudioNode = null;
const isOwnerViewer = () => sbOn() && sbSignedIn();

$("btn-call").onclick = () => {
  toggleDrawer("call-panel");
  const owner = isOwnerViewer();
  for (const id of ["call-mute-pc", "call-cam-pc"]) {
    $(id).classList.toggle("hidden", !owner);
    $(id).classList.toggle("flex", owner);
  }
};
function callSetStatus(t) { $("call-status").textContent = t; }
function callSend(o) { if (ws && ws.readyState === 1) ws.send(JSON.stringify(o)); }

async function callStart() {
  if (callState !== "idle") return;
  try {
    callStream = await navigator.mediaDevices.getUserMedia({ video: { width: 320, height: 240 }, audio: true });
  } catch (e) { toast("Camera/mic blocked — allow access to call."); return; }
  $("call-local-video").srcObject = callStream;
  callState = "calling";
  $("call-start").classList.add("hidden"); $("call-end").classList.remove("hidden");
  $("btn-call").classList.add("on");
  callSetStatus("Ringing the PC…");
  callSend({ t: "call-start", name: myProfile.display_name || "Guest" });
  // if the PC doesn't answer in 20s, tell the user why (old agent? offline?)
  clearTimeout(callRingTimer);
  callRingTimer = setTimeout(() => {
    if (callState === "calling") {
      callStop();
      callSetStatus("PC didn't answer");
      toast("No answer — is the v7.0+ agent running on the PC?");
    }
  }, 20000);
  // auto-start streaming; agent answers with call-accept
  startCallMedia();
}
function startCallMedia() {
  // camera frames: JPEG snapshots ~5fps as binary 0x04
  const vid = $("call-local-video"), cv = document.createElement("canvas");
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
function callStop() {
  callState = "idle";
  clearTimeout(callRingTimer);
  clearInterval(callFrameTimer); callFrameTimer = null;
  if (callAudioNode) { try { callAudioNode.disconnect(); } catch {} callAudioNode = null; }
  if (callAudioCtx) { try { callAudioCtx.close(); } catch {} callAudioCtx = null; }
  if (callStream) { callStream.getTracks().forEach((t) => t.stop()); callStream = null; }
  $("call-local-video").srcObject = null;
  $("call-start").classList.remove("hidden"); $("call-end").classList.add("hidden");
  $("btn-call").classList.remove("on");
  callSetStatus("Tap call to start");
}
$("call-start").onclick = callStart;
$("call-end").onclick = () => { callSend({ t: "call-end" }); callStop(); callSetStatus("Call ended"); };
$("call-mic").onclick = () => {
  callMicOn = !callMicOn;
  $("call-mic").textContent = callMicOn ? "🎤 Mic" : "🔇 Muted";
  callSend({ t: "av-state", cam: callCamOn, mic: callMicOn });
};
$("call-cam").onclick = () => {
  callCamOn = !callCamOn;
  $("call-cam").textContent = callCamOn ? "📷 Cam" : "🚫 Off";
  $("call-local-off").classList.toggle("hidden", callCamOn);
  $("call-local-off").classList.toggle("grid", !callCamOn);
  callSend({ t: "av-state", cam: callCamOn, mic: callMicOn });
};
$("call-mute-pc").onclick = () => {
  // owner viewer toggles the PC's mic
  pcMicOn = !pcMicOn;
  callSend({ t: "av-ctrl", target: "agent", mic: pcMicOn });
  updatePCCallUI();
};
$("call-cam-pc").onclick = () => {
  // owner viewer toggles the PC's camera
  pcCamOn = !pcCamOn;
  callSend({ t: "av-ctrl", target: "agent", cam: pcCamOn });
  updatePCCallUI();
};
let pcCamOn = true, pcMicOn = true;
function onCallSignal(m) {
  if (m.t === "call-accept" && callState === "calling") { callState = "incall"; callSetStatus("Connected — they can see & hear you"); }
  else if (m.t === "call-end" && callState !== "idle") { callStop(); callSetStatus("They ended the call"); toast("Call ended"); }
  else if (m.t === "call-decline" && callState === "calling") { callStop(); callSetStatus("They declined"); }
  else if (m.t === "av-state" && m.side === "agent") {
    pcCamOn = m.cam !== false; pcMicOn = m.mic !== false;
    updatePCCallUI();
  }
}
function updatePCCallUI() {
  const pip = $("call-pip");
  if (!pip) return;
  const ph = pip.querySelector(".pc-cam-placeholder");
  if (pcCamOn) { if (ph) ph.style.display = "none"; }
  else {
    $("call-remote-video").classList.add("hidden");
    if (ph) { ph.style.display = "grid"; ph.querySelector(".pc-cam-msg").textContent = "PC camera is off"; }
  }
  const muteBtn = $("call-mute-pc"), camBtn = $("call-cam-pc");
  muteBtn.innerHTML = `<span>${pcMicOn ? "🔇" : "🎤"}</span><span>${pcMicOn ? "Mute PC mic" : "Unmute PC mic"}</span>`;
  camBtn.innerHTML = `<span>📷</span><span>${pcCamOn ? "PC cam off" : "PC cam on"}</span>`;
}
// v8.1: make the PC PiP draggable
(function() {
  const pip = $("call-pip");
  if (!pip) return;
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

/* ---------- PWA ---------- */
if ("serviceWorker" in navigator) navigator.serviceWorker.register("sw.js").catch(() => {});
let deferredInstall = null;
window.addEventListener("beforeinstallprompt", (e) => { e.preventDefault(); deferredInstall = e; });
bindGestures();
boot();
