/*
 * SW Transcode Worker
 * --------------------
 * Small companion service to the SWRemote relay (same repo, separate Render
 * service). Takes a TeraBox download link, compresses the video with ffmpeg
 * ("compact" preset ~360p), and uploads the small file straight to Telegram.
 *
 * Why it exists: Telegram bots cap uploads at 50 MB and our Vercel server
 * only gets 60 s per file — big videos could only go out as download links.
 * Compressed on this worker, they land in the channel as playable videos.
 *
 * Protocol:
 *   POST /api/transcode  { key, dlink, ndus, fileName, chat_id, bot_token }
 *     -> 202 { ok:true, job_id, status:"queued" }   (or 4xx on bad input)
 *   GET  /api/jobs/<id>  -> { ok:true, job:{ id, status, detail } }
 *   GET  /api/health     -> { ok:true }
 *
 * Secrets (ndus cookie, bot token) arrive per job over HTTPS and are never
 * written to disk or logged. The shared `key` must match TRANSCODE_KEY.
 *
 * Run:  npm install && npm start        (env: PORT, TRANSCODE_KEY)
 * Build (Render): downloads a static ffmpeg binary into ./ffmpeg
 */

"use strict";

const http = require("http");
const fs = require("fs");
const os = require("os");
const path = require("path");
const crypto = require("crypto");
const { spawn } = require("child_process");

const PORT = parseInt(process.env.PORT || "8080", 10);
const KEY = process.env.TRANSCODE_KEY || "";
// ffmpeg binary: from the ffmpeg-static npm package (fetched at npm install),
// with a local ./ffmpeg as fallback.
const FFMPEG = (() => {
  try {
    return require("ffmpeg-static");
  } catch {
    return path.join(__dirname, "ffmpeg");
  }
})();
const UA =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36";
// Telegram Bot API base (overridable only for local protocol tests).
const TG_BASE = process.env.TG_API_BASE || "https://api.telegram.org";

// ---------- TeraWatch watchdog ----------
// The iPhone "TeraFetch Watch" shortcut pings /api/watch-ping every loop
// (~8 s). If the pings stop for over 150 s, iOS has almost certainly killed
// the shortcut — we ask TeraFetch (which holds the bot token) to post a
// "⚠️ TeraWatch stopped" alert in the channel. The pings also keep this
// service warm, so the alert fires long before Render's idle spin-down.
const WATCH_KEY = process.env.WATCH_KEY || "";
const TF_BASE = "https://terabox-fetch-sankstudent1-creates-projects.vercel.app";
const WATCH_GAP_MS = 150000;
const watch = { lastPing: 0, alerted: false };

setInterval(async () => {
  if (!WATCH_KEY || !watch.lastPing || watch.alerted) return;
  if (Date.now() - watch.lastPing < WATCH_GAP_MS) return;
  try {
    const r = await fetchTimeout(
      TF_BASE + "/api/watch-alert",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ key: WATCH_KEY }),
      },
      20000
    );
    const j = await r.json().catch(() => ({}));
    if (j && j.ok) watch.alerted = true; // re-armed by the next ping
  } catch {
    /* try again on the next tick */
  }
}, 30000);

const MAX_CONCURRENT = 10; // parallel TeraBox connections (aria2-style split)
const PART_SIZE = 2 * 1024 * 1024; // 2 MB parts
const JOB_TIMEOUT_MS = 40 * 60 * 1000; // 40 min hard cap per job

const jobs = new Map(); // id -> { id, status, detail, createdAt, fileName }
const queue = [];
let active = false;

function json(res, code, obj) {
  const b = JSON.stringify(obj);
  res.writeHead(code, {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(b),
  });
  res.end(b);
}

function readJson(req) {
  return new Promise((resolve, reject) => {
    let b = "";
    req.on("data", (c) => {
      b += c;
      if (b.length > 1024 * 1024) reject(new Error("body too large"));
    });
    req.on("end", () => {
      try {
        resolve(b ? JSON.parse(b) : {});
      } catch {
        reject(new Error("bad json"));
      }
    });
    req.on("error", reject);
  });
}

function isTeraBoxDlink(u) {
  try {
    return new URL(u).hostname.toLowerCase().includes("terabox");
  } catch {
    return false;
  }
}

async function fetchTimeout(url, opts = {}, ms = 25000) {
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), ms);
  try {
    return await fetch(url, { ...opts, signal: ctrl.signal });
  } finally {
    clearTimeout(t);
  }
}

/** True file size via 1-byte range probe (Content-Range). */
async function probeSize(dlink, ndus) {
  const r = await fetchTimeout(
    dlink,
    {
      headers: { Cookie: `ndus=${ndus}`, Range: "bytes=0-0", "User-Agent": UA },
      redirect: "follow",
    },
    20000
  );
  if (r.body) await r.body.cancel().catch(() => {});
  if (r.status !== 206 && r.status !== 200) throw new Error(`probe -> ${r.status}`);
  const m = /\/(\d+)\s*$/.exec(r.headers.get("content-range") || "");
  const size = m ? Number(m[1]) : Number(r.headers.get("content-length") || 0);
  if (!size) throw new Error("could not determine file size");
  return size;
}

/**
 * Stream one byte-range into the file, RESUMING where it stopped whenever
 * the connection stalls (aria2's "continue" behavior). Bytes already on
 * disk are never re-fetched, so a throttled, stall-prone CDN cannot churn
 * forever the way restart-from-zero retries did.
 */
async function downloadRange(fd, dlink, ndus, start, end, onBytes) {
  const want = end - start + 1;
  let got = 0;
  let attempts = 0;
  let noProgress = 0;
  while (got < want) {
    if (++attempts > 60 || noProgress >= 6) {
      throw new Error(`range ${start} stuck at ${got}/${want}`);
    }
    let progressed = false;
    try {
      const r = await fetchTimeout(
        dlink,
        {
          headers: {
            Cookie: `ndus=${ndus}`,
            Range: `bytes=${start + got}-${end}`,
            "User-Agent": UA,
          },
          redirect: "follow",
        },
        30000
      );
      if (r.status !== 206 || !r.body) {
        if (r.body) await r.body.cancel().catch(() => {});
        throw new Error(`range -> ${r.status}`);
      }
      const reader = r.body.getReader();
      for (;;) {
        let timer;
        const timeout = new Promise((_, rej) => {
          timer = setTimeout(() => rej(new Error("body stalled")), 30000);
        });
        let chunk;
        try {
          chunk = await Promise.race([reader.read(), timeout]);
        } finally {
          clearTimeout(timer);
        }
        if (chunk.done) break;
        fs.writeSync(fd, chunk.value, 0, chunk.value.length, start + got);
        got += chunk.value.length;
        progressed = true;
        onBytes(chunk.value.length);
      }
    } catch {
      // Stall / network error: loop resumes from `got` — nothing is lost.
    }
    noProgress = progressed ? 0 : noProgress + 1;
    if (got < want) await new Promise((r) => setTimeout(r, 400));
  }
}

/** Parallel resumable download straight to disk, with progress callback. */
async function downloadToFile(dlink, ndus, filePath, total, onProgress) {
  const nParts = Math.min(MAX_CONCURRENT, Math.max(2, Math.ceil(total / PART_SIZE)));
  const ranges = [];
  const per = Math.ceil(total / nParts);
  for (let s = 0; s < total; s += per) ranges.push([s, Math.min(s + per - 1, total - 1)]);

  const fd = fs.openSync(filePath, "w");
  try {
    fs.ftruncateSync(fd, total);
    let done = 0;
    const onBytes = (n) => {
      done += n;
      if (onProgress) onProgress(done, total);
    };
    await Promise.all(
      ranges.map(([s, e]) => downloadRange(fd, dlink, ndus, s, e, onBytes))
    );
    if (done !== total) throw new Error(`incomplete download ${done}/${total}`);
  } finally {
    fs.closeSync(fd);
  }
}

function runFfmpegArgs(args, msCap, label) {
  return new Promise((resolve, reject) => {
    const p = spawn(FFMPEG, args, { stdio: ["ignore", "pipe", "pipe"] });
    let stderr = "";
    p.stderr.on("data", (d) => {
      stderr += d;
      if (stderr.length > 4000) stderr = stderr.slice(-4000);
    });
    const kill = setTimeout(() => {
      p.kill("SIGKILL");
      reject(new Error(label + " timed out"));
    }, msCap);
    p.on("error", (e) => {
      clearTimeout(kill);
      reject(new Error("ffmpeg failed to start: " + e.message));
    });
    p.on("close", (code) => {
      clearTimeout(kill);
      if (code === 0) resolve();
      else reject(new Error("ffmpeg exit " + code + ": " + stderr.slice(-300)));
    });
  });
}

/** Read duration / video codec / container from ffmpeg's own banner. */
function probeMedia(inPath) {
  return new Promise((resolve) => {
    const p = spawn(FFMPEG, ["-hide_banner", "-i", inPath], { stdio: ["ignore", "pipe", "pipe"] });
    let err = "";
    p.stderr.on("data", (d) => {
      err += d;
      if (err.length > 6000) err = err.slice(-6000);
    });
    p.on("error", () => resolve({}));
    p.on("close", () => {
      const out = {};
      const dm = /Duration:\s*(\d+):(\d+):(\d+(?:\.\d+)?)/.exec(err);
      if (dm) out.duration = +dm[1] * 3600 + +dm[2] * 60 + +dm[3];
      const vm = /Video:\s*([a-z0-9]+)/i.exec(err);
      if (vm) out.codec = vm[1].toLowerCase();
      const fm = /Input #0,\s*(.+?),\s*from/.exec(err);
      if (fm) out.format = fm[1].toLowerCase();
      resolve(out);
    });
  });
}

/**
 * Encoder v2 plan. Telegram bots cannot upload >50 MB, so the bitrate is
 * chosen FROM the duration: output target ~42 MB, video bitrate clamped to
 * [110k, 900k]; very long videos (>40 min) drop to 240p so they still fit.
 * Unknown duration -> the old fixed 220k/48k plan.
 */
function encodePlan(duration) {
  if (!duration || duration <= 0) {
    return { height: 360, vKbps: 220, aKbps: 48, preset: "superfast" };
  }
  const targetKbps = Math.floor((42 * 1024 * 1024 * 8) / 1000 / duration);
  const aKbps = targetKbps >= 260 ? 48 : 40;
  const vKbps = Math.max(110, Math.min(900, targetKbps - aKbps));
  return { height: duration > 2400 ? 240 : 360, vKbps, aKbps, preset: "superfast" };
}

function buildEncodeArgs(inPath, outPath, plan) {
  return [
    "-y",
    "-i", inPath,
    "-vf", `scale=-2:${plan.height}:flags=fast_bilinear,fps=30`,
    "-c:v", "libx264",
    "-preset", plan.preset,
    "-b:v", `${plan.vKbps}k`,
    "-maxrate", `${Math.round(plan.vKbps * 1.5)}k`,
    "-bufsize", `${plan.vKbps * 2}k`,
    "-c:a", "aac",
    "-b:a", `${plan.aKbps}k`,
    "-movflags", "+faststart",
    outPath,
  ];
}

/** Repack into MP4 without re-encoding (h264 in MKV etc.) — seconds, lossless. */
function runRemux(inPath, outPath, msCap) {
  return runFfmpegArgs(
    ["-y", "-i", inPath, "-c", "copy", "-movflags", "+faststart", outPath],
    msCap,
    "remux"
  );
}

async function tgUpload(botToken, chatId, fileName, filePath, origSize, kind, height) {
  const stat = fs.statSync(filePath);
  const form = new FormData();
  form.append("chat_id", chatId);
  const buf = fs.readFileSync(filePath);
  const base = fileName.replace(/\.[^.]+$/, "");
  const sendName =
    kind === "original" ? fileName : kind === "remux" ? base + ".mp4" : base + "-compact.mp4";
  form.append("video", new Blob([buf], { type: "video/mp4" }), sendName);
  let caption;
  if (kind === "original") {
    caption = `🎬 <b>${esc(fileName)}</b>\n📦 Original ${fmtMB(stat.size)} · sent as-is — no compression needed`;
  } else if (kind === "remux") {
    caption = `🎬 <b>${esc(fileName)}</b>\n⚡ Repacked to MP4 · original quality · ${fmtMB(stat.size)}`;
  } else {
    caption = `🎬 <b>${esc(fileName)}</b>\n🗜️ Compact ${height || 360}p · ${fmtMB(stat.size)}`;
    if (origSize && origSize > stat.size) {
      const saved = Math.round((1 - stat.size / origSize) * 100);
      caption += `\n📦 Original ${fmtMB(origSize)} → saved ${saved}%`;
    }
  }
  form.append("caption", caption);
  form.append("parse_mode", "HTML");
  form.append("supports_streaming", "true");
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), 120000);
  try {
    const r = await fetch(`${TG_BASE}/bot${botToken}/sendVideo`, {
      method: "POST",
      body: form,
      signal: ctrl.signal,
    });
    const j = await r.json().catch(() => ({}));
    if (!j.ok) throw new Error(j.description || `Telegram HTTP ${r.status}`);
    return j.result.message_id;
  } finally {
    clearTimeout(t);
  }
}

/**
 * Status-card protocol: the caller (TeraFetch) posts a "compressing" card
 * (thumbnail + title + download link) and hands us its message id. We keep
 * the caption updated with progress, and when the compact video is ready we
 * delete the card and send the video in its place. On failure the card stays
 * (it carries the download link) with a failure caption.
 */
function esc(s) {
  return String(s || "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function fmtMB(n) {
  return n > 0 ? (n / 1048576).toFixed(1) + " MB" : "";
}

/** 10-cell unicode progress bar: ▰▰▰▰▱▱▱▱▱▱ */
function bar(pct) {
  const filled = Math.max(0, Math.min(10, Math.round(pct / 10)));
  return "▰".repeat(filled) + "▱".repeat(10 - filled);
}

function statusCaption(job, stageHtml) {
  let s = `🎬 <b>${esc(job.fileName)}</b>`;
  if (job.size) s += `\n📦 ${fmtMB(job.size)} · full file linked below`;
  s += `\n\n${stageHtml}`;
  if (job.fallback_link) {
    s += `\n\n⬇️ <a href="${esc(job.fallback_link)}">Download full file</a> · valid 7 days`;
  }
  return s.slice(0, 1024);
}

async function tgJson(job, method, params) {
  const r = await fetchTimeout(
    `${TG_BASE}/bot${job.bot_token}/${method}`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(params),
    },
    20000
  );
  return r.json().catch(() => ({}));
}

/** Update the status card's stage line. Best-effort and throttled: stage
 *  changes go out immediately; a failed edit (other than Telegram's benign
 *  "not modified") is retried once, so the card never lags a stage behind. */
async function setStage(job, stage, force) {
  if (!job.status_message_id || !job.bot_token) return;
  const now = Date.now();
  if (!force && job._stageText === stage) return;
  if (!force && job._lastEdit && now - job._lastEdit < 15000) return;
  job._stageText = stage;
  job._lastEdit = now;
  const caption = statusCaption(job, stage);
  const send = () =>
    job.status_is_photo
      ? tgJson(job, "editMessageCaption", {
          chat_id: job.chat_id, message_id: job.status_message_id, caption, parse_mode: "HTML",
        })
      : tgJson(job, "editMessageText", {
          chat_id: job.chat_id, message_id: job.status_message_id, text: caption,
          parse_mode: "HTML", disable_web_page_preview: true,
        });
  try {
    const r = await send();
    if (r && r.ok === false && !/not modified/i.test(r.description || "")) {
      await new Promise((res) => setTimeout(res, 5000));
      await send().catch(() => {});
    }
  } catch {
    /* status updates never break the job */
  }
}

async function deleteStatusCard(job) {
  if (!job.status_message_id || !job.bot_token) return;
  try {
    await tgJson(job, "deleteMessage", {
      chat_id: job.chat_id, message_id: job.status_message_id,
    });
  } catch {
    /* the card may already be gone — fine */
  }
}

/**
 * DIAGNOSTIC: hunt for TeraBox's own compressed stream (the fast path).
 * Runs the logged-in share-list flow and reports — key names only, never
 * secret values — whether sign/timestamp appear, and whether the streaming
 * endpoint answers with an m3u8. Result lands on job.probe; it never
 * blocks or breaks the job.
 */
async function probeStream(job) {
  const out = {};
  const headers = { "User-Agent": UA, Cookie: `ndus=${job.ndus}` };
  try {
    if (!job.surl) return { skip: "no surl" };
    const pageRes = await fetchTimeout(
      `https://www.terabox.com/wap/share/filelist?surl=${encodeURIComponent(job.surl)}`,
      { headers },
      20000
    );
    const html = await pageRes.text();
    const scripts = [...html.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/gi)].map((m) => m[1] || "");
    const target = scripts.find(
      (s) => /^\s*try\s*\{\s*eval\(decodeURIComponent/.test(s) && s.includes("%22")
    );
    const jsToken = target ? target.split("%22")[1] : null;
    out.jsToken = jsToken ? "yes" : "no";
    if (!jsToken) return out;
    const params = new URLSearchParams({
      app_id: "250528",
      jsToken,
      shorturl: job.surl,
      root: "1",
      num: "100",
      page: "1",
    });
    const lr = await fetchTimeout(`https://www.terabox.com/share/list?${params}`, { headers }, 20000);
    const data = await lr.json().catch(() => null);
    if (!data) return { ...out, list: "unparsable" };
    out.listKeys = Object.keys(data).join(",");
    out.sign = data.sign ? "yes" : "no";
    out.timestamp = data.timestamp ? "yes" : "no";
    const item =
      (data.list || []).find((x) => String(x.fs_id) === String(job.fs_id)) ||
      (data.list || [])[0];
    if (!item) return { ...out, item: "none" };
    out.itemKeys = Object.keys(item).join(",");
    const base = {
      path: item.path || "",
      app_id: "250528",
      clienttype: "0",
      fid: String(item.fs_id),
      type: "M3U8_AUTO_360",
      web: "1",
    };
    const variants = [["cookie-only", base]];
    if (data.sign && data.timestamp) {
      variants.unshift(["with-sign", { ...base, sign: data.sign, timestamp: String(data.timestamp) }]);
    }
    for (const [name, q] of variants) {
      try {
        const sr = await fetchTimeout(
          `https://www.terabox.com/api/streaming?${new URLSearchParams(q)}`,
          { headers },
          20000
        );
        const txt = await sr.text();
        out[`stream_${name}`] =
          `${sr.status} ${(sr.headers.get("content-type") || "").split(";")[0]} ` +
          txt.slice(0, 80).replace(/\s+/g, " ");
      } catch (e) {
        out[`stream_${name}`] = "ERR " + String((e && e.message) || e).slice(0, 60);
      }
    }
  } catch (e) {
    out.error = String((e && e.message) || e).slice(0, 120);
  }
  return out;
}

async function processJob(job) {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "swt-"));
  const inPath = path.join(tmpDir, "in.mp4");
  const outPath = path.join(tmpDir, "out.mp4");
  // Time caps live in fetchPart (30 s/part) and runFfmpeg; the whole job is
  // also bounded by Render's own limits. Secrets are scrubbed in `finally`.
  try {
    await setStage(job, "🗜️ <b>Compressing</b> · ⏳ starting…", true);
    job.status = "probing";
    job.probe = await probeStream(job);
    job.status = "downloading";
    const total = await probeSize(job.dlink, job.ndus);
    if (total > 1024 * 1024 * 1024) throw new Error("file over 1 GB — refusing");
    let nextMilestone = 0;
    await downloadToFile(job.dlink, job.ndus, inPath, total, (doneB, totB) => {
      const pct = Math.floor((doneB / totB) * 100);
      job.detail = `downloading ${pct}%`;
      if (pct >= nextMilestone) {
        nextMilestone = pct - (pct % 25) + 25;
        setStage(job, `🗜️ <b>Compressing</b> · ⬇️ downloading\n${bar(pct)} ${pct}%`, true);
      }
    });

    // ---- Encoder v2: cheapest path that delivers a playable video ----
    const media = await probeMedia(inPath);
    const FAST_MAX = 47 * 1024 * 1024; // safely under Telegram's 50 MB bot cap
    const mp4Like = /mp4|mov/.test(media.format || "");
    let messageId;
    let kind = "compact";
    let sendPath = outPath;

    if (total <= FAST_MAX && media.codec === "h264" && mp4Like) {
      // Already a Telegram-ready file — skip encoding entirely.
      kind = "original";
      sendPath = inPath;
      job.enc = { path: "original", duration: Math.round(media.duration || 0), codec: media.codec || "" };
      job.status = "uploading";
      job.detail = "sending original (no encode needed)";
      await setStage(job, "⚡ <b>Already Telegram-ready</b> · ⬆️ sending the original…", true);
      messageId = await tgUpload(job.bot_token, job.chat_id, job.fileName, sendPath, job.size, kind);
    } else if (total <= FAST_MAX && media.codec === "h264") {
      // Right codec, wrong wrapper — repack to MP4 without re-encoding.
      kind = "remux";
      job.enc = { path: "remux", duration: Math.round(media.duration || 0), codec: media.codec || "" };
      job.status = "compressing";
      job.detail = "remuxing";
      await setStage(job, "⚡ <b>Repacking to MP4</b> · original quality, no re-encode…", true);
      await runRemux(inPath, outPath, 180000);
      job.status = "uploading";
      job.detail = "uploading";
      await setStage(job, "⚡ <b>Repacking to MP4</b> · ⬆️ sending to Telegram…", true);
      messageId = await tgUpload(job.bot_token, job.chat_id, job.fileName, sendPath, job.size, kind);
    } else {
      // Full re-encode, duration-aware plan (+ one oversize retry at 70%).
      const plan = encodePlan(media.duration);
      job.enc = { path: "encode", duration: Math.round(media.duration || 0), ...plan };
      job.status = "compressing";
      job.detail = "compressing";
      await setStage(job, "🗜️ <b>Compressing</b> · 🎞️ encoding compact video…", true);
      // Long encodes must never LOOK stuck: refresh the stage with elapsed
      // time every 2 minutes while ffmpeg runs.
      const encStart = Date.now();
      const heartbeat = setInterval(() => {
        const mins = Math.max(1, Math.round((Date.now() - encStart) / 60000));
        setStage(job, `🗜️ <b>Compressing</b> · 🎞️ encoding compact video… ${mins} min`, true);
      }, 120000);
      try {
        await runFfmpegArgs(buildEncodeArgs(inPath, outPath, plan), JOB_TIMEOUT_MS - 5 * 60 * 1000, "transcode");
        const outSize = fs.existsSync(outPath) ? fs.statSync(outPath).size : 0;
        if (outSize > 49.5 * 1024 * 1024) {
          const plan2 = { ...plan, vKbps: Math.max(90, Math.round(plan.vKbps * 0.7)) };
          job.enc.retry = plan2.vKbps;
          await runFfmpegArgs(buildEncodeArgs(inPath, outPath, plan2), JOB_TIMEOUT_MS - 5 * 60 * 1000, "transcode");
        }
      } finally {
        clearInterval(heartbeat);
      }
      job.status = "uploading";
      job.detail = "uploading";
      await setStage(job, "🗜️ <b>Compressing</b> · ⬆️ sending to Telegram…", true);
      messageId = await tgUpload(job.bot_token, job.chat_id, job.fileName, sendPath, job.size, kind, plan.height);
    }

    // The video is in — now replace the status card with it.
    await deleteStatusCard(job);

    job.status = "done";
    job.detail = `video sent via ${kind} (message ${messageId})`;
  } catch (e) {
    job.status = "failed";
    job.detail = String((e && e.message) || e).slice(0, 300);
    // Leave the card in place (it carries the download link), marked failed.
    await setStage(job, "❌ <b>Compression failed</b> — the full file is at the link below.", true);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
    // scrub secrets
    job.dlink = "";
    job.ndus = "";
    job.bot_token = "";
    job.fallback_link = "";
    active = false;
    pump();
  }
}

function pump() {
  if (active || !queue.length) return;
  const job = queue.shift();
  active = true;
  job.status = "working";
  processJob(job);
}

// ---------- HTTP ----------
const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  try {
    if (req.method === "GET" && url.pathname === "/api/health") {
      return json(res, 200, {
        ok: true,
        ffmpeg: fs.existsSync(FFMPEG),
        queued: queue.length,
        active,
        watch: {
          armed: !!WATCH_KEY,
          seen: !!watch.lastPing,
          lastPingAgoSec: watch.lastPing ? Math.round((Date.now() - watch.lastPing) / 1000) : null,
          alerted: watch.alerted,
        },
      });
    }
    if (req.method === "GET" && url.pathname === "/api/watch-ping") {
      if (!WATCH_KEY || url.searchParams.get("k") !== WATCH_KEY) {
        return json(res, 403, { ok: false, error: "bad watch key" });
      }
      watch.lastPing = Date.now();
      watch.alerted = false; // a living Watch re-arms the watchdog
      return json(res, 200, { ok: true });
    }
    if (req.method === "GET" && url.pathname === "/api/jobs") {
      const list = [...jobs.values()]
        .map(({ dlink, ndus, bot_token, fallback_link, ...safe }) => safe)
        .slice(-20)
        .reverse();
      return json(res, 200, { ok: true, jobs: list });
    }
    if (req.method === "GET" && url.pathname.startsWith("/api/jobs/")) {
      const job = jobs.get(url.pathname.split("/").pop());
      if (!job) return json(res, 404, { ok: false, error: "unknown job" });
      const { dlink, ndus, bot_token, fallback_link, ...safe } = job;
      return json(res, 200, { ok: true, job: safe });
    }
    if (req.method === "POST" && url.pathname === "/api/transcode") {
      const b = await readJson(req);
      if (!KEY || b.key !== KEY) return json(res, 403, { ok: false, error: "bad key" });
      const { dlink, ndus, fileName, chat_id, bot_token, surl, fs_id,
        status_message_id, status_is_photo, fallback_link, size } = b;
      if (!dlink || !isTeraBoxDlink(dlink) || !ndus || !chat_id || !bot_token) {
        return json(res, 400, { ok: false, error: "missing dlink/ndus/chat_id/bot_token" });
      }
      if (!fs.existsSync(FFMPEG)) {
        return json(res, 503, { ok: false, error: "ffmpeg not installed on this worker" });
      }
      const id = crypto.randomBytes(8).toString("hex");
      const job = {
        id,
        status: "queued",
        detail: "",
        createdAt: Date.now(),
        fileName: String(fileName || "video.mp4").slice(0, 120),
        dlink,
        ndus,
        surl: String(surl || ""),
        fs_id: String(fs_id || ""),
        chat_id: String(chat_id),
        bot_token,
        status_message_id: Number(status_message_id || 0),
        status_is_photo: !!status_is_photo,
        fallback_link: String(fallback_link || "").slice(0, 500),
        size: Number(size || 0),
      };
      jobs.set(id, job);
      queue.push(job);
      if (jobs.size > 200) {
        const first = jobs.keys().next().value;
        jobs.delete(first);
      }
      pump();
      return json(res, 202, { ok: true, job_id: id, status: job.status });
    }
    return json(res, 404, { ok: false, error: "not found" });
  } catch (e) {
    return json(res, 500, { ok: false, error: String((e && e.message) || e).slice(0, 200) });
  }
});

server.listen(PORT, () => console.log(`swtranscode on :${PORT}`));
