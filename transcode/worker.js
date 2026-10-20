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

function runFfmpeg(inPath, outPath, msCap) {
  return new Promise((resolve, reject) => {
    // Compact preset: 360p, ~220k video + 48k audio ≈ 270 kbps total.
    // 6 min ≈ 12 MB — small enough to fly into Telegram, watchable on phones.
    const args = [
      "-y",
      "-i",
      inPath,
      "-vf",
      "scale=-2:360",
      "-c:v",
      "libx264",
      "-preset",
      "veryfast",
      "-b:v",
      "220k",
      "-maxrate",
      "330k",
      "-bufsize",
      "660k",
      "-c:a",
      "aac",
      "-b:a",
      "48k",
      "-movflags",
      "+faststart",
      outPath,
    ];
    const p = spawn(FFMPEG, args, { stdio: ["ignore", "pipe", "pipe"] });
    let stderr = "";
    p.stderr.on("data", (d) => {
      stderr += d;
      if (stderr.length > 4000) stderr = stderr.slice(-4000);
    });
    const kill = setTimeout(() => {
      p.kill("SIGKILL");
      reject(new Error("transcode timed out"));
    }, msCap);
    p.on("error", (e) => {
      clearTimeout(kill);
      reject(new Error("ffmpeg failed to start: " + e.message));
    });
    p.on("close", (code) => {
      clearTimeout(kill);
      if (code === 0 && fs.existsSync(outPath)) resolve();
      else reject(new Error("ffmpeg exit " + code + ": " + stderr.slice(-300)));
    });
  });
}

async function tgUpload(botToken, chatId, fileName, filePath) {
  const stat = fs.statSync(filePath);
  const form = new FormData();
  form.append("chat_id", chatId);
  const buf = fs.readFileSync(filePath);
  form.append("video", new Blob([buf], { type: "video/mp4" }), fileName.replace(/\.[^.]+$/, "") + "-compact.mp4");
  form.append("caption", `${fileName}  •  compact ${(stat.size / 1048576).toFixed(1)} MB`);
  form.append("supports_streaming", "true");
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), 120000);
  try {
    const r = await fetch(`https://api.telegram.org/bot${botToken}/sendVideo`, {
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
    job.status = "probing";
    job.probe = await probeStream(job);
    job.status = "downloading";
    const total = await probeSize(job.dlink, job.ndus);
    if (total > 1024 * 1024 * 1024) throw new Error("file over 1 GB — refusing");
    await downloadToFile(job.dlink, job.ndus, inPath, total, (doneB, totB) => {
      job.detail = `downloading ${Math.floor((doneB / totB) * 100)}%`;
    });

    job.status = "compressing";
    await runFfmpeg(inPath, outPath, JOB_TIMEOUT_MS - 5 * 60 * 1000);

    job.status = "uploading";
    const messageId = await tgUpload(job.bot_token, job.chat_id, job.fileName, outPath);

    job.status = "done";
    job.detail = `compact video sent (message ${messageId})`;
  } catch (e) {
    job.status = "failed";
    job.detail = String((e && e.message) || e).slice(0, 300);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
    // scrub secrets
    job.dlink = "";
    job.ndus = "";
    job.bot_token = "";
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
      });
    }
    if (req.method === "GET" && url.pathname === "/api/jobs") {
      const list = [...jobs.values()]
        .map(({ dlink, ndus, bot_token, ...safe }) => safe)
        .slice(-20)
        .reverse();
      return json(res, 200, { ok: true, jobs: list });
    }
    if (req.method === "GET" && url.pathname.startsWith("/api/jobs/")) {
      const job = jobs.get(url.pathname.split("/").pop());
      if (!job) return json(res, 404, { ok: false, error: "unknown job" });
      const { dlink, ndus, bot_token, ...safe } = job;
      return json(res, 200, { ok: true, job: safe });
    }
    if (req.method === "POST" && url.pathname === "/api/transcode") {
      const b = await readJson(req);
      if (!KEY || b.key !== KEY) return json(res, 403, { ok: false, error: "bad key" });
      const { dlink, ndus, fileName, chat_id, bot_token, surl, fs_id } = b;
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
