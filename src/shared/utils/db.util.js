import fs from 'node:fs';
import path from 'node:path';
import { CACHE_CONFIG } from '../config.js';
import { logger } from './logger.util.js';

const isBun = typeof Bun !== 'undefined';

// ---------------------------------------------------------------------------
// In-memory LRU layers in front of SQLite. Every sync findExact/findExisting
// scan blocks the main thread (Bun's sqlite is synchronous), so on a 2-core
// box the hot catalog must be served from RAM, never from disk.
// ---------------------------------------------------------------------------
const DB_LRU_SIZE = Number(process.env.CACHE_DB_LRU_SIZE) || 20000;
const DB_LRU_TTL_MS = 15 * 60 * 1000;
const DB_LRU_NEGATIVE_TTL_MS = 30 * 1000;
const DB_CONTENT_LRU_ENTRIES = Number(process.env.CACHE_DB_CONTENT_LRU_ENTRIES) || 500;
const DB_CONTENT_LRU_MAX_BYTES = Number(process.env.CACHE_DB_CONTENT_LRU_MAX_BYTES) || 48 * 1024 * 1024;

class MemoryLRU {
    constructor(maxEntries, ttlMs, negativeTtlMs) {
        this.maxEntries = maxEntries;
        this.ttlMs = ttlMs;
        this.negativeTtlMs = negativeTtlMs;
        this.map = new Map();
    }

    get(key) {
        const entry = this.map.get(key);
        if (!entry) return undefined;
        if (Date.now() > entry.expiresAt) {
            this.map.delete(key);
            return undefined;
        }
        // Refresh LRU order
        this.map.delete(key);
        this.map.set(key, entry);
        return entry.value;
    }

    set(key, value, negative = false) {
        const ttl = negative ? this.negativeTtlMs : this.ttlMs;
        if (this.map.has(key)) this.map.delete(key);
        else if (this.map.size >= this.maxEntries) {
            const oldest = this.map.keys().next().value;
            if (oldest !== undefined) this.map.delete(oldest);
        }
        this.map.set(key, { value, expiresAt: Date.now() + ttl });
    }

    delete(key) {
        this.map.delete(key);
    }

    clear() {
        this.map.clear();
    }

    get size() {
        return this.map.size;
    }
}

class ContentLRU {
    constructor(maxEntries, maxBytes) {
        this.maxEntries = maxEntries;
        this.maxBytes = maxBytes;
        this.totalBytes = 0;
        this.map = new Map();
    }

    get(key) {
        const entry = this.map.get(key);
        if (!entry) return undefined;
        this.map.delete(key);
        this.map.set(key, entry);
        return entry.value;
    }

    set(key, value) {
        const bytes = typeof value === 'string' ? value.length : 0;
        if (this.map.has(key)) {
            const prev = this.map.get(key);
            this.totalBytes -= prev.bytes;
            this.map.delete(key);
        }
        this.map.set(key, { value, bytes });
        this.totalBytes += bytes;

        while ((this.map.size > this.maxEntries || this.totalBytes > this.maxBytes) && this.map.size > 0) {
            const oldest = this.map.keys().next().value;
            if (oldest === undefined) break;
            const evicted = this.map.get(oldest);
            this.totalBytes -= evicted.bytes;
            this.map.delete(oldest);
        }
    }

    delete(key) {
        const entry = this.map.get(key);
        if (entry) {
            this.totalBytes -= entry.bytes;
            this.map.delete(key);
        }
    }

    clear() {
        this.map.clear();
        this.totalBytes = 0;
    }

    get size() {
        return this.map.size;
    }
}

// findExact: key -> row | null (null = cached negative)
const exactCache = new MemoryLRU(DB_LRU_SIZE / 4, DB_LRU_TTL_MS, DB_LRU_NEGATIVE_TTL_MS);
// findExisting: key -> array of rows | [] ([] = cached empty)
const existingCache = new MemoryLRU(DB_LRU_SIZE, DB_LRU_TTL_MS, DB_LRU_NEGATIVE_TTL_MS);
// getRowById: key = id; evicted on updateRowContent
const contentCache = new ContentLRU(DB_CONTENT_LRU_ENTRIES, DB_CONTENT_LRU_MAX_BYTES);

function exactCacheKey(folderIDs, mimeType, songISRC, songPlatformId) {
    return `${mimeType}|${[...folderIDs].sort().join(',')}|${songISRC || ''}|${songPlatformId || ''}`;
}

function existingCacheKey(folderIDs, mimeType, keywords) {
    const kw = Array.isArray(keywords) ? [...keywords].sort().join('~') : '';
    return `${mimeType}|${[...folderIDs].sort().join(',')}|${kw}`;
}

export function clearDBMemoryCaches() {
    exactCache.clear();
    existingCache.clear();
    contentCache.clear();
}

class SQLiteDriver {
    constructor(db, isBunEnv) {
        this.db = db;
        this.isBunEnv = isBunEnv;
        this.stmtCache = new Map();
    }

    _prepare(sql) {
        let cached = this.stmtCache.get(sql);
        if (cached) return cached;

        if (this.isBunEnv) {
            const q = this.db.query(sql);
            cached = {
                get: (...params) => q.get(...params),
                all: (...params) => q.all(...params),
                run: (...params) => q.run(...params),
            };
        } else {
            const stmt = this.db.prepare(sql);
            cached = {
                get: (...params) => stmt.get(...params),
                all: (...params) => stmt.all(...params),
                run: (...params) => stmt.run(...params),
            };
        }
        this.stmtCache.set(sql, cached);
        return cached;
    }

    findExact(folderIDs, mimeType, songISRC, songPlatformId) {
        if (!folderIDs || folderIDs.length === 0) return null;
        if (!songISRC && !songPlatformId) return null;

        const foldersPlaceholder = folderIDs.map(() => '?').join(',');

        if (songISRC) {
            try {
                const stmt = this._prepare(`
                    SELECT id, gdrive_id, file_name, folder_id, mime_type, title, artist, album, duration, isrc, platform_id
                    FROM lyrics_cache
                    WHERE folder_id IN (${foldersPlaceholder}) AND mime_type = ? AND isrc = ?
                    LIMIT 1
                `);
                const row = stmt.get(...folderIDs, mimeType, songISRC);
                if (row) return row;
            } catch (err) {
                logger.error('[DB] SQLite findExact isrc error:', err);
            }
        }

        if (songPlatformId) {
            try {
                const stmt = this._prepare(`
                    SELECT id, gdrive_id, file_name, folder_id, mime_type, title, artist, album, duration, isrc, platform_id
                    FROM lyrics_cache
                    WHERE folder_id IN (${foldersPlaceholder}) AND mime_type = ? AND platform_id = ?
                    LIMIT 1
                `);
                const row = stmt.get(...folderIDs, mimeType, songPlatformId);
                if (row) return row;
            } catch (err) {
                logger.error('[DB] SQLite findExact platform_id error:', err);
            }
        }

        return null;
    }

    findExisting(folderIDs, mimeType, keywords) {
        if (!folderIDs || folderIDs.length === 0) return [];
        const foldersPlaceholder = folderIDs.map(() => '?').join(',');
        let query = `
            SELECT id, gdrive_id, file_name, folder_id, mime_type, title, artist, album, duration, isrc, platform_id
            FROM lyrics_cache
            WHERE folder_id IN (${foldersPlaceholder})
              AND mime_type = ?
        `;
        const params = [...folderIDs, mimeType];

        if (keywords && keywords.length > 0) {
            const likeClauses = keywords.map(() => `file_name LIKE ?`).join(' AND ');
            query += ` AND (${likeClauses})`;
            params.push(...keywords.map(k => `%${k}%`));
        }

        query += ` LIMIT 100`;

        try {
            const stmt = this._prepare(query);
            return stmt.all(...params);
        } catch (err) {
            logger.error('[DB] SQLite findExisting error:', err);
            return [];
        }
    }

    saveRow(row) {
        const query = `
            INSERT INTO lyrics_cache (
                id, gdrive_id, file_name, folder_id, mime_type, content,
                title, artist, album, duration, isrc, platform_id,
                created_at, updated_at
            ) VALUES (
                ?, ?, ?, ?, ?, ?,
                ?, ?, ?, ?, ?, ?,
                ?, ?
            )
            ON CONFLICT(file_name) DO UPDATE SET
                gdrive_id = COALESCE(excluded.gdrive_id, lyrics_cache.gdrive_id),
                content = excluded.content,
                updated_at = excluded.updated_at
        `;
        
        const now = Date.now();
        const params = [
            row.id,
            row.gdrive_id || null,
            row.file_name,
            row.folder_id || null,
            row.mime_type || null,
            row.content || null,
            row.title || null,
            row.artist || null,
            row.album || null,
            row.duration !== undefined ? row.duration : null,
            row.isrc || null,
            row.platform_id || null,
            now,
            now
        ];

        try {
            const stmt = this._prepare(query);
            stmt.run(...params);
        } catch (err) {
            logger.error('[DB] SQLite saveRow error:', err);
        }
    }

    getRowById(id) {
        try {
            const stmt = this._prepare('SELECT * FROM lyrics_cache WHERE id = ?');
            return stmt.get(id) || null;
        } catch (err) {
            logger.error('[DB] SQLite getRowById error:', err);
            return null;
        }
    }

    updateRowContent(id, content) {
        try {
            const stmt = this._prepare('UPDATE lyrics_cache SET content = ?, updated_at = ? WHERE id = ?');
            stmt.run(content, Date.now(), id);
        } catch (err) {
            logger.error('[DB] SQLite updateRowContent error:', err);
        }
    }
}

/**
 * Bun-only SQLite driver that runs in a dedicated worker thread, so the
 * synchronous bun:sqlite queries never block the main event loop (the culprit
 * behind bottleneck behavior on low-end hosts). Methods are async RPC.
 */
class BunWorkerDriver {
    constructor() {
        this.worker = null;
        this.pending = new Map();
        this._seq = 0;
        this._initPromise = null;
    }

    _createWorker() {
        const worker = new Worker(new URL('./db.worker.js', import.meta.url), { type: 'module' });
        worker.onmessage = (e) => {
            const { id, ok, value, error } = e.data;
            const p = this.pending.get(id);
            if (!p) return;
            this.pending.delete(id);
            if (ok) p.resolve(value);
            else p.reject(new Error(error));
        };
        worker.onerror = (e) => this._failAll(e?.message || 'worker script error');
        worker.onclose = () => this._failAll('worker closed');
        return worker;
    }

    _failAll(message) {
        this.worker = null;
        this._initPromise = null;
        const err = new Error(`[DB] Worker failed: ${message}`);
        for (const p of this.pending.values()) p.reject(err);
        this.pending.clear();
    }

    _rpc(method, args) {
        if (!this.worker) throw new Error('[DB] Worker not initialized');
        const id = ++this._seq;
        return new Promise((resolve, reject) => {
            this.pending.set(id, { resolve, reject });
            this.worker.postMessage({ id, method, args });
        });
    }

    async _ensureInit(dbPath) {
        if (this.worker && this._initPromise) return this._initPromise;
        this._failAll('recreating worker');
        this.worker = this._createWorker();
        this._initPromise = this._rpc('init', [dbPath]).catch((err) => {
            this._failAll(err.message);
            throw err;
        });
        return this._initPromise;
    }

    async findExact(folderIDs, mimeType, songISRC, songPlatformId) {
        return this._rpc('findExact', [folderIDs, mimeType, songISRC, songPlatformId]);
    }

    async findExisting(folderIDs, mimeType, keywords) {
        return this._rpc('findExisting', [folderIDs, mimeType, keywords]);
    }

    async saveRow(row) {
        return this._rpc('saveRow', [row]);
    }

    async getRowById(id) {
        return this._rpc('getRowById', [id]);
    }

    async updateRowContent(id, content) {
        return this._rpc('updateRowContent', [id, content]);
    }
}

class JSONFileDriver {
    constructor(filePath) {
        this.filePath = filePath;
        this.data = [];
        this.writePromise = Promise.resolve();
    }

    async load() {
        try {
            if (fs.existsSync(this.filePath)) {
                const raw = fs.readFileSync(this.filePath, 'utf8');
                this.data = JSON.parse(raw) || [];
            }
        } catch (err) {
            logger.error('[DB] JSONFileDriver load error:', err);
            this.data = [];
        }
    }

    async save() {
        this.writePromise = this.writePromise.then(async () => {
            try {
                fs.writeFileSync(this.filePath, JSON.stringify(this.data, null, 2), 'utf8');
            } catch (err) {
                logger.error('[DB] JSONFileDriver save error:', err);
            }
        });
        return this.writePromise;
    }

    findExact(folderIDs, mimeType, songISRC, songPlatformId) {
        return this.data.find(row => {
            if (!folderIDs.includes(row.folder_id)) return false;
            if (row.mime_type !== mimeType) return false;
            if (songISRC && row.isrc === songISRC) return true;
            if (songPlatformId && row.platform_id === songPlatformId) return true;
            return false;
        }) || null;
    }

    findExisting(folderIDs, mimeType, keywords) {
        return this.data.filter(row => {
            if (!folderIDs.includes(row.folder_id)) return false;
            if (row.mime_type !== mimeType) return false;
            if (keywords && keywords.length > 0) {
                const fileNameLower = row.file_name.toLowerCase();
                for (const kw of keywords) {
                    if (!fileNameLower.includes(kw.toLowerCase())) return false;
                }
            }
            return true;
        });
    }

    saveRow(row) {
        const idx = this.data.findIndex(r => r.file_name === row.file_name);
        const now = Date.now();
        if (idx !== -1) {
            const existing = this.data[idx];
            this.data[idx] = {
                ...existing,
                gdrive_id: row.gdrive_id !== undefined ? row.gdrive_id : existing.gdrive_id,
                content: row.content,
                updated_at: now
            };
        } else {
            this.data.push({
                ...row,
                created_at: now,
                updated_at: now
            });
        }
        this.save();
    }

    getRowById(id) {
        return this.data.find(r => r.id === id) || null;
    }

    updateRowContent(id, content) {
        const idx = this.data.findIndex(r => r.id === id);
        if (idx !== -1) {
            this.data[idx].content = content;
            this.data[idx].updated_at = Date.now();
            this.save();
        }
    }
}

class NullDriver {
    findExact() { return null; }
    findExisting() { return []; }
    saveRow() {}
    getRowById() { return null; }
    updateRowContent() {}
}

let dbInstance = null;
let initialized = false;
let initPromise = null;

function applyPragmas(nativeDb, isBunEnv) {
    const pragmas = [
        'PRAGMA journal_mode = WAL;',
        'PRAGMA synchronous = NORMAL;',
        'PRAGMA temp_store = MEMORY;',
        'PRAGMA cache_size = -32000;',
        'PRAGMA mmap_size = 33554432;',
        'PRAGMA busy_timeout = 5000;'
    ];
    for (const pragma of pragmas) {
        try {
            if (isBunEnv) nativeDb.run(pragma);
            else nativeDb.exec(pragma);
        } catch (err) {
            logger.debug('[DB] Pragma apply failed:', pragma, err.message);
        }
    }
}

async function getDB() {
    if (initialized) return dbInstance;
    if (initPromise) return initPromise;

    initPromise = (async () => {
        if (!CACHE_CONFIG.DB_ENABLED) {
            dbInstance = new NullDriver();
            initialized = true;
            return dbInstance;
        }

        const dbPath = CACHE_CONFIG.DB_PATH;
        try {
            const dir = path.dirname(dbPath);
            if (dir && dir !== '.' && !fs.existsSync(dir)) {
                fs.mkdirSync(dir, { recursive: true });
            }

            // 1. Try to load Bun's native sqlite in a worker thread (non-blocking)
            if (isBun && process.env.CACHE_DB_WORKER !== 'false') {
                try {
                    const workerDriver = new BunWorkerDriver();
                    await workerDriver._ensureInit(dbPath);
                    dbInstance = workerDriver;
                    logger.log(`[DB] Local database initialized using bun:sqlite (worker thread) at: ${dbPath}`);
                    initialized = true;
                    return dbInstance;
                } catch (workerErr) {
                    logger.debug('[DB] bun:sqlite worker init failed, falling back to sync:', workerErr.message);
                }
            }

            // 2. Try to load Bun's native sqlite synchronously (main thread)
            if (isBun) {
                try {
                    const { Database } = await import('bun:sqlite');
                    const nativeDb = new Database(dbPath, { create: true });
                    
                    applyPragmas(nativeDb, true);
                    
                    nativeDb.run(`
                        CREATE TABLE IF NOT EXISTS lyrics_cache (
                            id TEXT PRIMARY KEY,
                            gdrive_id TEXT,
                            file_name TEXT UNIQUE,
                            folder_id TEXT,
                            mime_type TEXT,
                            content TEXT,
                            title TEXT,
                            artist TEXT,
                            album TEXT,
                            duration REAL,
                            isrc TEXT,
                            platform_id TEXT,
                            created_at INTEGER,
                            updated_at INTEGER
                        )
                    `);
                    
                    nativeDb.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_folder_mime ON lyrics_cache(folder_id, mime_type)`);
                    nativeDb.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_isrc ON lyrics_cache(isrc)`);
                    nativeDb.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_platform_id ON lyrics_cache(platform_id)`);
                    nativeDb.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_exact ON lyrics_cache(folder_id, mime_type, isrc, platform_id)`);
                    nativeDb.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_folder_mime_isrc ON lyrics_cache(folder_id, mime_type, isrc)`);
                    nativeDb.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_folder_mime_platform_id ON lyrics_cache(folder_id, mime_type, platform_id)`);
                    nativeDb.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_updated_at ON lyrics_cache(updated_at)`);

                    dbInstance = new SQLiteDriver(nativeDb, true);
                    logger.log(`[DB] Local database initialized using bun:sqlite at: ${dbPath}`);
                    initialized = true;
                    return dbInstance;
                } catch (bunErr) {
                    logger.debug('[DB] bun:sqlite load failed:', bunErr.message);
                }
            }

            // 2. Try to load Node's native sqlite (available in Node 22.5+)
            try {
                const { DatabaseSync } = await import('node:sqlite');
                const nativeDb = new DatabaseSync(dbPath);
                
                applyPragmas(nativeDb, false);
                
                nativeDb.exec(`
                    CREATE TABLE IF NOT EXISTS lyrics_cache (
                        id TEXT PRIMARY KEY,
                        gdrive_id TEXT,
                        file_name TEXT UNIQUE,
                        folder_id TEXT,
                        mime_type TEXT,
                        content TEXT,
                        title TEXT,
                        artist TEXT,
                        album TEXT,
                        duration REAL,
                        isrc TEXT,
                        platform_id TEXT,
                        created_at INTEGER,
                        updated_at INTEGER
                    )
                `);
                
                nativeDb.exec(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_folder_mime ON lyrics_cache(folder_id, mime_type)`);
                nativeDb.exec(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_isrc ON lyrics_cache(isrc)`);
                nativeDb.exec(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_platform_id ON lyrics_cache(platform_id)`);
                nativeDb.exec(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_exact ON lyrics_cache(folder_id, mime_type, isrc, platform_id)`);
                nativeDb.exec(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_folder_mime_isrc ON lyrics_cache(folder_id, mime_type, isrc)`);
                nativeDb.exec(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_folder_mime_platform_id ON lyrics_cache(folder_id, mime_type, platform_id)`);
                nativeDb.exec(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_updated_at ON lyrics_cache(updated_at)`);

                dbInstance = new SQLiteDriver(nativeDb, false);
                logger.log(`[DB] Local database initialized using node:sqlite at: ${dbPath}`);
                initialized = true;
                return dbInstance;
            } catch (nativeErr) {
                logger.debug('[DB] node:sqlite load failed, falling back to JSON file:', nativeErr.message);
            }

            // 3. Fallback to pure-JS JSON file-based database
            const jsonPath = dbPath.endsWith('.db') ? dbPath.slice(0, -3) + '.json' : dbPath + '.json';
            const jsonDb = new JSONFileDriver(jsonPath);
            await jsonDb.load();
            dbInstance = jsonDb;
            logger.log(`[DB] Local database initialized using JSON fallback at: ${jsonPath}`);
            initialized = true;
            return dbInstance;
        } catch (err) {
            logger.error('[DB] Failed to initialize any local database. Local DB cache is disabled.', err);
            dbInstance = new NullDriver();
            initialized = true;
            return dbInstance;
        }
    })();

    return initPromise;
}

export const db = {
    async findExact(folderIDs, mimeType, songISRC, songPlatformId) {
        if (!folderIDs || folderIDs.length === 0) return null;
        const key = exactCacheKey(folderIDs, mimeType, songISRC, songPlatformId);
        const cached = exactCache.get(key);
        if (cached !== undefined) return cached;

        const driver = await getDB();
        const row = await driver.findExact(folderIDs, mimeType, songISRC, songPlatformId);
        exactCache.set(key, row, row === null);
        return row;
    },
    async findExisting(folderIDs, mimeType, keywords) {
        if (!folderIDs || folderIDs.length === 0) return [];
        const key = existingCacheKey(folderIDs, mimeType, keywords);
        const cached = existingCache.get(key);
        if (cached !== undefined) return cached;

        const driver = await getDB();
        const rows = await driver.findExisting(folderIDs, mimeType, keywords) || [];
        const isEmpty = rows.length === 0;
        const copy = rows.slice();
        existingCache.set(key, copy, isEmpty);
        return rows;
    },
    async saveRow(row) {
        const driver = await getDB();
        return await driver.saveRow(row);
    },
    async getRowById(id) {
        if (typeof id === 'string' && id.startsWith('db:')) {
            id = id.slice(3);
        }
        if (!id) return null;

        const cached = contentCache.get(id);
        if (cached !== undefined) return cached;

        const driver = await getDB();
        const row = await driver.getRowById(id);
        // ContentLRU is byte-budgeted (evicts by total bytes), so large
        // syllable-sync blobs are safe to cache and gain the most from it.
        if (row) contentCache.set(id, row);
        return row;
    },
    async updateRowContent(id, content) {
        if (typeof id === 'string' && id.startsWith('db:')) {
            id = id.slice(3);
        }
        contentCache.delete(id);
        const driver = await getDB();
        return await driver.updateRowContent(id, content);
    }
};
