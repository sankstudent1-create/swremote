/* SWRemote console service worker — offline shell cache + offline fallback */
const CACHE = "swremote-v4";
const ASSETS = [
  "./", "./index.html", "./offline.html", "./style.css", "./app.js",
  "./manifest.json", "./logo.png", "./login-bg.jpg",
  "./icon-192.png", "./icon-512.png",
  "./icon-maskable-192.png", "./icon-maskable-512.png",
];
self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(ASSETS)).then(() => self.skipWaiting()));
});
self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});
self.addEventListener("fetch", (e) => {
  if (e.request.method !== "GET") return;
  e.respondWith(
    caches.match(e.request).then((cached) => {
      if (cached) return cached;
      return fetch(e.request).then((res) => {
        // never cache API or websocket traffic, only the app shell
        return res;
      }).catch(() => {
        if (e.request.mode === "navigate") return caches.match("./offline.html");
        throw new Error("offline");
      });
    })
  );
});
