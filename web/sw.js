const CACHE = 'remoto-v2';
const SHELL = ['/', '/app.js', '/app.css', '/manifest.webmanifest', '/icon-192.png'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});
self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys().then((ks) => Promise.all(ks.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});
// Rede primeiro; o cache só cobre o shell quando o PC está fora do ar.
self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.pathname.startsWith('/api/')) return;
  e.respondWith(fetch(e.request, { cache: 'no-cache' }).catch(() => caches.match(e.request)));
});

self.addEventListener('push', (e) => {
  let d = {};
  try { d = e.data.json(); } catch { d = { title: 'lich', body: e.data && e.data.text() }; }
  e.waitUntil(self.registration.showNotification(d.title || 'lich', {
    body: d.body || '', tag: d.tag, renotify: true, icon: '/icon-192.png', badge: '/icon-192.png',
  }));
});
self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((cs) => {
    for (const c of cs) if ('focus' in c) return c.focus();
    return self.clients.openWindow('/');
  }));
});
