self.addEventListener("install", (e) => {
  e.waitUntil(caches.open("pg-app-v1").then((c) => c.addAll(["/app/", "/app/index.html", "/app/app.js", "/app/styles.css"])));
});

self.addEventListener("fetch", (e) => {
  e.respondWith(caches.match(e.request).then((r) => r || fetch(e.request)));
});

self.addEventListener("push", (event) => {
  let data = { title: "PG / Hostel", body: "You have a new update." };
  if (event.data) {
    try {
      data = event.data.json();
    } catch (_) {
      data = { title: "PG / Hostel", body: event.data.text() };
    }
  }
  const options = {
    body: data.body || "",
    icon: data.icon || "/app/icon.png",
    badge: data.badge || "/app/icon.png",
    data: data.url || "/app/",
    vibrate: [200, 100, 200],
  };
  event.waitUntil(self.registration.showNotification(data.title || "PG / Hostel", options));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const urlToOpen = event.notification.data || "/app/";
  event.waitUntil(
    clients.matchAll({ type: "window", includeUncontrolled: true }).then((windowClients) => {
      for (let client of windowClients) {
        if (client.url.includes(urlToOpen) && "focus" in client) {
          return client.focus();
        }
      }
      if (clients.openWindow) {
        return clients.openWindow(urlToOpen);
      }
    })
  );
});
