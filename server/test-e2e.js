/* SWRemote end-to-end protocol test: fake agent + fake viewer against the real server */
const fs = require("fs");
const http = require("http");
const { spawn } = require("child_process");
const WebSocket = require("/home/hatch/workspace/swremote/server/node_modules/ws");

const PORT = 18099;
const FRAME = fs.readFileSync("/tmp/testframe.jpg");
const results = [];
const ok = (name, cond) => { results.push([cond ? "PASS" : "FAIL", name]); if (!cond) process.exitCode = 1; };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const once = (ws, pred, timeout = 4000) => new Promise((res, rej) => {
  const t = setTimeout(() => rej(new Error("timeout")), timeout);
  const h = (raw, isBin) => {
    const buf = Buffer.isBuffer(raw) ? raw : Buffer.from(raw); // normalize ArrayBuffer (browser-style binaryType)
    let m = null; try { m = JSON.parse(buf.toString()); } catch {}
    if (pred(m, buf, isBin)) { clearTimeout(t); ws.off("message", h); res({ m, raw: buf, isBin }); }
  };
  ws.on("message", h);
});

(async () => {
  const srv = spawn("node", ["/home/hatch/workspace/swremote/server/server.js"], { env: { ...process.env, PORT: String(PORT) } });
  srv.stderr.on("data", (d) => process.stderr.write(d));
  await wait(1200);

  const get = (p) => new Promise((res, rej) => http.get({ port: PORT, path: p }, (r) => {
    let b = ""; r.on("data", (c) => b += c); r.on("end", () => res(JSON.parse(b)));
  }).on("error", rej));

  // --- fake agent ---
  const agent = new WebSocket(`ws://localhost:${PORT}/ws`);
  await new Promise((r) => agent.on("open", r));
  agent.send(JSON.stringify({ t: "register", id: "TESTDEV1", name: "Test PC", pin: "999999", platform: "windows", screen: { w: 640, h: 400 } }));
  console.log("step: register"); const reg = await once(agent, (m) => m && m.t === "registered");
  ok("agent registers", reg.m.id === "TESTDEV1");

  // --- fake viewer, correct PIN ---
  const viewer = new WebSocket(`ws://localhost:${PORT}/ws`);
  viewer.binaryType = "arraybuffer";
  await new Promise((r) => viewer.on("open", r));
  viewer.send(JSON.stringify({ t: "join", id: "TESTDEV1", pin: "999999" }));
  console.log("step: join"); const joined = await once(viewer, (m) => m && m.t === "joined");
  ok("viewer joins with correct PIN (online)", joined.m.online === true && joined.m.id === "TESTDEV1");

  // --- video frames flow agent -> viewer ---
  for (let i = 0; i < 3; i++) agent.send(Buffer.concat([Buffer.from([0x01]), FRAME]));
  console.log("step: frame"); const frame = await once(viewer, (m, raw, isBin) => isBin && raw[0] === 0x01);
  ok("viewer receives video frame", frame.raw.length > 1000 && frame.raw.slice(1, 3).toString("hex") === FRAME.slice(0, 2).toString("hex"));

  // --- input flows viewer -> agent ---
  console.log("step: input"); const inputP = once(agent, (m) => m && m.t === "input" && m.k === "move");
  viewer.send(JSON.stringify({ t: "input", k: "move", x: 0.5, y: 0.25 }));
  const inp = await inputP;
  ok("input event relayed to agent", inp.m.x === 0.5 && inp.m.y === 0.25);

  // --- chat viewer -> agent ---
  console.log("step: chat"); const chatP = once(agent, (m) => m && m.t === "chat");
  viewer.send(JSON.stringify({ t: "chat", text: "hello pc" }));
  ok("chat relayed to agent", (await chatP).m.text === "hello pc");

  // --- file chunk viewer -> agent ---
  console.log("step: file"); const metaP = once(agent, (m) => m && m.t === "file_meta");
  const chunkP = once(agent, (m, raw, isBin) => isBin && raw[0] === 0x02);
  viewer.send(JSON.stringify({ t: "file_meta", id: "fabc", name: "note.txt", size: 11 }));
  const fid = Buffer.from("fabc");
  viewer.send(Buffer.concat([Buffer.from([0x02, fid.length]), fid, Buffer.from("hello world")]));
  const meta = await metaP, chk = await chunkP;
  ok("file meta + chunk relayed to agent", meta.m.name === "note.txt" && chk.raw.slice(2 + 4).toString() === "hello world");

  // --- wrong PIN rejected ---
  const bad = new WebSocket(`ws://localhost:${PORT}/ws`);
  await new Promise((r) => bad.on("open", r));
  bad.send(JSON.stringify({ t: "join", id: "TESTDEV1", pin: "000000" }));
  console.log("step: badpin"); const err = await once(bad, (m) => m && m.t === "error");
  ok("wrong PIN rejected", /wrong pin/.test(err.m.msg));
  bad.close();

  // --- HTTP APIs ---
  console.log("step: http"); const health = await get("/api/health");
  ok("health endpoint", health.ok === true && health.online === 1);
  const devs = await get("/api/devices");
  const d = devs.devices.find((x) => x.id === "TESTDEV1");
  ok("device list shows online device (no PIN leaked)", d && d.online === true && !("pinHash" in d) && !("pin" in d));

  // --- agent disconnect notifies viewer ---
  agent.close();
  const gone = await once(viewer, (m) => m && m.t === "agent_gone");
  ok("viewer told when agent goes offline", !!gone.m);

  viewer.close();
  await wait(300);
  const devs2 = await get("/api/devices");
  ok("device shows offline after disconnect", devs2.devices.find((x) => x.id === "TESTDEV1").online === false);

  srv.kill();
  console.log("\n" + results.map(([s, n]) => `${s}  ${n}`).join("\n"));
  const fails = results.filter(([s]) => s === "FAIL").length;
  console.log(fails ? `\n${fails} FAILURES` : "\nALL TESTS PASSED");
})().catch((e) => { console.error("TEST ERROR:", e.message); process.exit(1); });
