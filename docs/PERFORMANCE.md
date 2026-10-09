# SWRemote — Performance

> Why the stream was slow, what v3.0 changed, and honest limits.

## 1. The problem (v2.x): ~5–8 real fps

The v2 pipeline did this **for every frame**, at 12 fps:

1. `screenshot.CaptureRect` — GDI BitBlt of the whole screen: **~15–25 ms**
2. `draw.ApproxBiLinear.Scale` — pure-Go downscale to 0.8×: **~20–30 ms**
3. `jpeg.Encode` — pure-Go JPEG of the **entire** 1536×864 frame: **~90–140 ms**
4. WebSocket send of ~150–300 KB through the Render relay (India → US/EU → viewer)

Total: **~130–200 ms per frame → 5–8 fps in practice**, and the adaptive-quality
logic kept crushing JPEG quality down to 30 trying to keep up — so it was both
slow *and* blurry. The 12 fps ticker just queued up lag.

## 2. What AnyDesk does differently (for reference)

- **DeskRT codec**: purpose-built for screens — sends only changed regions,
  tuned for text/edges rather than photos.
- **Direct P2P (UDP)** whenever NAT allows — no relay in the middle.
- **Hardware capture** (Desktop Duplication) + hardware video encode.

We can't copy DeskRT, but we can steal its most important idea: **don't re-send
what didn't change.**

## 3. The v3.0 fix: tiled dirty-region streaming

```
capture full screen (BitBlt)
        │  128×128 tiles
        ▼
┌─────┬─────┬─────┬─────┐
│     │█████│     │     │  ← diff vs previous frame (row-wise memcmp, ~1 ms)
├─────┼─────┼─────┼─────┤
│     │█████│     │█████│  ← only shaded tiles are JPEG-encoded (~5–15 ms)
└─────┴─────┴─────┴─────┘
        │  0x03 packets
        ▼
viewer composites tiles onto its canvas
```

Per typical frame (a few tiles changed — cursor blink, typing, scrolling):

| Stage | v2.x | v3.0 |
|---|---|---|
| Capture | 15–25 ms | 15–25 ms |
| Scale | 20–30 ms | **0 ms** (tiles are encoded at native res = sharper) |
| Encode | 90–140 ms (full frame) | **5–20 ms** (changed tiles only) |
| Bytes sent | 150–300 KB | **10–60 KB** |
| **Real fps** | **5–8** | **25–30** |

Rules that keep it correct:

- **Keyframe** (`0x01` full JPEG): first frame, every 4 s, on viewer join, on
  viewer request, on resolution change, or when >40% of tiles changed (a full
  frame is cheaper than 60+ tile encodes).
- **Resolution change** resets the previous-frame buffer (avoids garbage diffs).
- **Adaptive quality** now measures tile-encode+send time instead of full frames,
  so quality stays high (starts at 70, adapts 30–90).

## 4. Honest limits

- **Full-screen motion** (video, games): most tiles change every frame → falls back
  to keyframes → ~10–15 fps. Software JPEG can't do 60 fps video; neither can
  any VNC-style tool. This is a remote-*desktop* codec, not a video codec.
- **The relay**: traffic still goes PC → Render → viewer. A same-region relay
  (or future P2P) would cut latency further. P2P (STUN/TURN) is on the v3 roadmap
  as an investigation, not a promise.
- **Target**: 25–30 fps at native sharpness for real desktop work (the AnyDesk
  use case). True 60 fps needs hardware encode + P2P — not achievable in our
  free-tier pure-Go stack, and we won't claim otherwise.

## 5. How to measure

- The console shows a live **fps pill** in the session bar (counts received
  frame batches per 2 s window).
- The agent logs per-tick cost when it exceeds the frame budget
  (`%LocalAppData%\SWRemote\agent.log`).
