import '../shared/utils/patch-caches.js';
import app from './app.js';
import { SERVER } from '../shared/config.js';

// A single unhandled rejection/uncaught exception must never take down the whole
// host. Log it and keep serving; individual requests already fail gracefully.
if (typeof process !== 'undefined') {
  process.on('unhandledRejection', (reason) => {
    console.error('[Process] Unhandled promise rejection (ignored):', reason?.message || reason);
  });
  process.on('uncaughtException', (err) => {
    console.error('[Process] Uncaught exception (ignored):', err?.message || err);
  });
}

const port = SERVER.PORT;
console.log(`Server is running on port ${port}`);

const MEMORY_WATCHDOG_MB = Number(process.env.MEMORY_WATCHDOG_MB) || 3072;
const MEMORY_WATCHDOG_INTERVAL_MS = 10_000;
async function runMemoryWatchdog() {
  try {
    const rss = process.memoryUsage().rss;
    const rssMB = Math.round(rss / 1024 / 1024);
    if (rssMB > MEMORY_WATCHDOG_MB) {
      console.error(`[Memory] RSS ${rssMB}MB exceeded ${MEMORY_WATCHDOG_MB}MB limit - shedding in-memory caches.`);

      const { clearDBMemoryCaches } = await import('../shared/utils/db.util.js');
      clearDBMemoryCaches();

      const { CacheStorage } = await import('../shared/utils/kv.emulator.js');
      if (globalThis.caches && globalThis.caches instanceof CacheStorage) {
        await globalThis.caches.clearAll();
      }

      if (typeof globalThis.gc === 'function') globalThis.gc();
      else if (typeof Bun !== 'undefined') {
        try { (await import('bun:jsc')).gc(true); } catch {}
      }
      console.error(`[Memory] After shedding caches: RSS ${Math.round(process.memoryUsage().rss / 1024 / 1024)}MB`);
    }
  } catch (err) {
    console.error('[Memory] Watchdog error:', err?.message || err);
  }
}
setInterval(runMemoryWatchdog, MEMORY_WATCHDOG_INTERVAL_MS);

if (typeof Bun !== 'undefined') {
  Bun.serve({
    fetch: app.fetch,
    port,
    idleTimeout: 60,
    maxRequestBodySize: 1024 * 1024 * 5,
  });
  
  if (process.env.ENABLE_MANUAL_GC === 'true') {
    try {
      const { gc } = require('bun:jsc');
      setInterval(() => {
        try { gc(false); } catch {}
      }, 5 * 60 * 1000);
    } catch (e) {}
  }
} else {
  const { serve } = await import('@hono/node-server');
  serve({
    fetch: (req, env) => app.fetch(req, env),
    port
  });
}
