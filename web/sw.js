self.addEventListener("install", (e) => {
  e.waitUntil(caches.open("pg-app-v1").then((c) => c.addAll(["/app/", "/app/index.html", "/app/app.js", "/app/styles.css"])));
});
self.addEventListener("fetch", (e) => {
  e.respondWith(caches.match(e.request).then((r) => r || fetch(e.request)));
});
