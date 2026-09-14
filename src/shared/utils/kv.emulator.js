import NodeCache from 'node-cache';

const MAX_KEYS = Number(process.env.MAX_CACHE_KEYS) || 2000;
const MAX_CACHE_BYTES = Number(process.env.MAX_CACHE_BYTES) || 96 * 1024 * 1024; // 96 MB hard cap (fits 4 GB hosts)
const MAX_BODY_BYTES = 1572864; // 1.5 MB for syllable sync lyrics support

function getByteLength(str) {
    if (typeof Buffer !== 'undefined' && Buffer.byteLength) {
        return Buffer.byteLength(str, 'utf8');
    }
    return str.length;
}

export class Cache {
    constructor(maxKeys = MAX_KEYS) {
        this.maxKeys = maxKeys;
        this.totalBytes = 0;
        this.cache = new NodeCache({ stdTTL: 3600, checkperiod: 60, useClones: false });
        this._queue = new Map();
        this.cache.on('del', (k) => this._dequeue(k));
        this.cache.on('expired', (k) => this._dequeue(k));
    }

    _dequeue(key) {
        const entry = this._queue.get(key);
        if (entry) {
            this.totalBytes = Math.max(0, this.totalBytes - entry.bytes);
            this._queue.delete(key);
        }
    }

    _evictOldest() {
        const oldest = this._queue.keys().next().value;
        if (oldest) this.cache.del(oldest);
    }

    _key(request) {
        if (typeof request === 'string') return `GET:${request}`;
        return `${request.method}:${request.url}`;
    }

    async match(request) {
        const key = this._key(request);
        const cached = this.cache.get(key);
        if (!cached) return undefined;
        try {
            return new Response(cached.body, { status: cached.status, headers: new Headers(cached.headers) });
        } catch {
            return undefined;
        }
    }

    async put(request, response) {
        const key = this._key(request);
        if (response.status !== 200) return;
        if (typeof request !== 'string' && (request.headers.has('authorization') || request.headers.has('cookie'))) return;
        try {
            const clone = response.clone();
            const body = await clone.text();
            const bodyBytes = getByteLength(body);
            if (bodyBytes > MAX_BODY_BYTES) return;
            if (clone.headers.has('set-cookie')) return;

            const cc = clone.headers.get('Cache-Control');
            let ttl = 3600;
            if (cc) {
                const m = cc.match(/max-age=(\d+)/);
                if (m) ttl = parseInt(m[1], 10);
            }

            // Evict previous entry with the same key before accounting bytes
            this._dequeue(key);

            const headers = [...clone.headers.entries()].filter(([name]) => name.toLowerCase() !== 'set-cookie');
            if (this.cache.set(key, { body, status: clone.status, headers }, ttl)) {
                const entry = { bytes: bodyBytes };
                this._queue.set(key, entry);
                this.totalBytes += bodyBytes;
            }

            // Enforce caps: key count AND total response bytes (prevents RAM blowup on 4 GB hosts)
            while (this._queue.size > this.maxKeys) {
                this._evictOldest();
            }
            while (this.totalBytes > MAX_CACHE_BYTES && this._queue.size > 0) {
                this._evictOldest();
            }
        } catch (e) {
            console.error(`[Cache] put error for ${key}:`, e);
        }
    }

    async delete(request) {
        this.cache.del(this._key(request));
        return true;
    }

    clear() {
        this.cache.flushAll();
        this._queue.clear();
        this.totalBytes = 0;
    }
}

export class CacheStorage {
    constructor() {
        this.cacheMap = new Map();
    }

    async open(cacheName) {
        if (!this.cacheMap.has(cacheName)) {
            this.cacheMap.set(cacheName, new Cache());
        }
        return this.cacheMap.get(cacheName);
    }

    async delete(cacheName) {
        return this.cacheMap.delete(cacheName);
    }

    async clearAll() {
        for (const cache of this.cacheMap.values()) {
            cache.clear();
        }
    }

    async keys() {
        return Array.from(this.cacheMap.keys());
    }

    async has(cacheName) {
        return this.cacheMap.has(cacheName);
    }
}
