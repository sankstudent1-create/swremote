"""
SWRemote Windows Agent v1
Runs on the Windows PC you want to control remotely.
- Registers with the relay server (device ID + PIN)
- Streams the screen as JPEG frames over WebSocket
- Applies remote mouse/keyboard input from viewers
- Receives files, chat messages, remote lock command

Install:  pip install -r requirements.txt
Run:      python agent.py
"""
import asyncio
import ctypes
import io
import json
import os
import random
import socket
import sys
import threading
import time
import traceback

CONFIG_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json")

# ---------- config ----------
def load_config():
    cfg = {
        "server": "ws://localhost:8080/ws",   # change to your relay, e.g. wss://swremote.onrender.com/ws
        "device_id": "",
        "name": socket.gethostname(),
        "pin": "123456",                       # change this! 6+ digit access PIN
        "fps": 8,
        "quality": 55,                         # JPEG quality 10-90
        "scale": 0.75,                         # capture scale (lower = faster)
    }
    if os.path.exists(CONFIG_PATH):
        try:
            cfg.update(json.load(open(CONFIG_PATH)))
        except Exception as e:
            print("config.json unreadable, using defaults:", e)
    if not cfg["device_id"]:
        # AnyDesk-style 9 digit id: "123 456 789"
        cfg["device_id"] = "".join(random.choice("123456789") for _ in range(9))
    try:
        json.dump(cfg, open(CONFIG_PATH, "w"), indent=2)
    except Exception:
        pass
    return cfg

CONFIG = load_config()

# ---------- optional platform deps (Windows) ----------
try:
    import mss
    from PIL import Image
    HAS_CAPTURE = True
except Exception:
    HAS_CAPTURE = False

try:
    from pynput.mouse import Controller as MouseController, Button as MouseButton
    from pynput.keyboard import Controller as KeyController, Key
    HAS_INPUT = True
except Exception:
    HAS_INPUT = False

try:
    import websockets
    HAS_WS = True
except Exception:
    HAS_WS = False
    print("ERROR: 'websockets' package missing. Run: pip install -r requirements.txt")
    sys.exit(1)

mouse = MouseController() if HAS_INPUT else None
keyboard = KeyController() if HAS_INPUT else None

SPECIAL_KEYS = {
    "enter": Key.enter, "tab": Key.tab, "backspace": Key.backspace, "delete": Key.delete,
    "esc": Key.esc, "escape": Key.esc, "space": Key.space, "shift": Key.shift,
    "ctrl": Key.ctrl, "alt": Key.alt, "up": Key.up, "down": Key.down,
    "left": Key.left, "right": Key.right, "home": Key.home, "end": Key.end,
    "pageup": Key.page_up, "pagedown": Key.page_down, "f1": Key.f1, "f2": Key.f2,
    "f3": Key.f3, "f4": Key.f4, "f5": Key.f5, "f6": Key.f6, "f7": Key.f7,
    "f8": Key.f8, "f9": Key.f9, "f10": Key.f10, "f11": Key.f11, "f12": Key.f12,
    "win": Key.cmd, "cmd": Key.cmd, "caps": Key.caps_lock,
}

BIN_FRAME = b"\x01"
BIN_FILE = b"\x02"

# ---------- screen capture ----------
def grab_frame():
    """Return (jpeg_bytes, w, h) or (None, 0, 0)."""
    if not HAS_CAPTURE:
        return None, 0, 0
    try:
        with mss.mss() as sct:
            mon = sct.monitors[1]  # primary monitor
            shot = sct.grab(mon)
            img = Image.frombytes("RGB", shot.size, shot.bgra, "raw", "BGRX")
            sw, sh = int(shot.width * CONFIG["scale"]), int(shot.height * CONFIG["scale"])
            if (sw, sh) != img.size:
                img = img.resize((sw, sh), Image.BILINEAR)
            buf = io.BytesIO()
            img.save(buf, "JPEG", quality=CONFIG["quality"])
            return buf.getvalue(), sw, sh
    except Exception as e:
        print("capture error:", e)
        return None, 0, 0

# ---------- remote input ----------
def apply_input(msg):
    if not HAS_INPUT:
        return
    try:
        k = msg.get("k")
        sw, sh = msg.get("sw", 0), msg.get("sh", 0)  # sender's notion of screen size (unused; we use ratios)
        if k == "move":
            # x,y are 0..1 ratios of the remote screen
            w = ctypes.windll.user32.GetSystemMetrics(0)
            h = ctypes.windll.user32.GetSystemMetrics(1)
            mouse.position = (int(msg["x"] * w), int(msg["y"] * h))
        elif k in ("down", "up"):
            btn = {"left": MouseButton.left, "right": MouseButton.right, "middle": MouseButton.middle}.get(msg.get("b", "left"), MouseButton.left)
            if k == "down": mouse.press(btn)
            else: mouse.release(btn)
        elif k == "scroll":
            mouse.scroll(int(msg.get("dx", 0)), int(msg.get("dy", 0)))
        elif k == "key":
            name = str(msg.get("key", ""))
            down = bool(msg.get("down", True))
            key = SPECIAL_KEYS.get(name.lower(), name if len(name) == 1 else None)
            if key is None:
                return
            if down: keyboard.press(key)
            else: keyboard.release(key)
        elif k == "type":
            keyboard.type(str(msg.get("text", ""))[:200])
    except Exception as e:
        print("input error:", e)

# ---------- helpers ----------
def notify(title, text):
    def _box():
        try:
            ctypes.windll.user32.MessageBoxW(0, text, title, 0x40)
        except Exception:
            pass
    threading.Thread(target=_box, daemon=True).start()

DOWNLOAD_DIR = os.path.join(os.path.expanduser("~"), "Downloads", "SWRemote")
os.makedirs(DOWNLOAD_DIR, exist_ok=True)
incoming_files = {}  # file_id -> {fh, name, left}

def handle_file_meta(msg):
    fid = msg.get("id"); name = os.path.basename(msg.get("name", "file.bin"))
    size = int(msg.get("size", 0))
    safe = "".join(c for c in name if c.isalnum() or c in "._- ")[:120] or "file.bin"
    path = os.path.join(DOWNLOAD_DIR, safe)
    incoming_files[fid] = {"fh": open(path, "wb"), "path": path, "left": size}
    print(f"Receiving file {safe} ({size} bytes) -> {path}")

def handle_file_chunk(data):
    # binary layout: 0x02 | file_id(utf8, 32B padded?) — simpler: JSON header impossible in binary,
    # so the sender first sent file_meta with id, and chunks carry: 0x02 + id_len(1B) + id + payload
    if len(data) < 3: return
    id_len = data[1]
    fid = data[2:2 + id_len].decode("utf8", "ignore")
    payload = data[2 + id_len:]
    rec = incoming_files.get(fid)
    if not rec: return
    rec["fh"].write(payload)
    rec["left"] -= len(payload)
    if rec["left"] <= 0:
        rec["fh"].close()
        print("File saved:", rec["path"])
        notify("SWRemote", "File received: " + os.path.basename(rec["path"]))
        del incoming_files[fid]

# ---------- main loop ----------
async def run_agent():
    url = CONFIG["server"]
    print(f"SWRemote agent starting — device {CONFIG['device_id']} ({CONFIG['name']})")
    print(f"Connecting to {url} ...")
    backoff = 2
    while True:
        try:
            async with websockets.connect(url, max_size=8 * 1024 * 1024, ping_interval=20) as ws:
                print("Connected. Registering...")
                await ws.send(json.dumps({
                    "t": "register", "id": CONFIG["device_id"], "name": CONFIG["name"],
                    "pin": CONFIG["pin"], "platform": "windows",
                }))
                backoff = 2
                loop = asyncio.get_event_loop()
                last_frame = 0.0
                interval = 1.0 / max(1, min(20, CONFIG["fps"]))

                async def sender():
                    nonlocal last_frame
                    while True:
                        now = time.time()
                        if now - last_frame >= interval:
                            last_frame = now
                            data, w, h = await loop.run_in_executor(None, grab_frame)
                            if data:
                                try:
                                    await ws.send(BIN_FRAME + data)
                                except Exception:
                                    return
                        await asyncio.sleep(0.01)

                async def pinger():
                    while True:
                        await asyncio.sleep(20)
                        try: await ws.send(json.dumps({"t": "ping"}))
                        except Exception: return

                send_task = asyncio.create_task(sender())
                ping_task = asyncio.create_task(pinger())
                try:
                    async for raw in ws:
                        if isinstance(raw, bytes):
                            if raw[:1] == BIN_FILE:
                                handle_file_chunk(raw[1:])
                            continue
                        try: msg = json.loads(raw)
                        except Exception: continue
                        t = msg.get("t")
                        if t == "registered":
                            print(f"Registered as {msg['id']}. Viewers online: {msg.get('viewers', 0)}")
                            print(f">>> Your SWRemote ID is: {CONFIG['device_id']}   PIN: {CONFIG['pin']}")
                        elif t == "input":
                            await loop.run_in_executor(None, apply_input, msg)
                        elif t == "quality":
                            CONFIG["quality"] = max(10, min(90, int(msg.get("q", CONFIG["quality"]))))
                            CONFIG["scale"] = max(0.25, min(1.0, float(msg.get("scale", CONFIG["scale"]))))
                            print("quality ->", CONFIG["quality"], "scale ->", CONFIG["scale"])
                        elif t == "chat":
                            print("CHAT:", msg.get("text", ""))
                            notify("SWRemote message", str(msg.get("text", ""))[:200])
                        elif t == "file_meta":
                            handle_file_meta(msg)
                        elif t == "lock":
                            print("Remote lock requested")
                            try: ctypes.windll.user32.LockWorkStation()
                            except Exception: pass
                        elif t == "viewer_joined":
                            notify("SWRemote", f"Someone connected ({msg.get('viewers', 1)} viewer(s))")
                        elif t == "viewer_left":
                            pass
                finally:
                    send_task.cancel(); ping_task.cancel()
        except Exception as e:
            print(f"Connection lost ({e}). Retrying in {backoff}s...")
            await asyncio.sleep(backoff)
            backoff = min(30, backoff * 2)

if __name__ == "__main__":
    if not HAS_CAPTURE:
        print("WARNING: screen capture libs missing (mss/Pillow). Install: pip install -r requirements.txt")
    if not HAS_INPUT:
        print("WARNING: input libs missing (pynput). Remote control will not work.")
    try:
        asyncio.run(run_agent())
    except KeyboardInterrupt:
        print("Stopped.")
