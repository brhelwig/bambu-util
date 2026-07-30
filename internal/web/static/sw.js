// Service worker for notifications. It caches nothing: the page is served
// no-cache on purpose, and a stale printer status is worse than a slow one.

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));

self.addEventListener("push", event => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    // A message that isn't ours still deserves to surface rather than vanish.
  }
  const title = data.title || "P1S";
  const tag = data.tag || undefined;
  event.waitUntil(self.registration.showNotification(title, {
    body: data.body || "",
    tag,
    // Repeated reminders about one condition replace each other rather than
    // stacking, but should still buzz — otherwise a silent replacement looks
    // like nothing happened.
    renotify: Boolean(tag),
    icon: "/icon-192.png",
    badge: "/icon-192.png",
  }));
});

self.addEventListener("notificationclick", event => {
  event.notification.close();
  event.waitUntil((async () => {
    const open = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const client of open) {
      if (client.url.startsWith(self.registration.scope)) return client.focus();
    }
    return self.clients.openWindow("/");
  })());
});
