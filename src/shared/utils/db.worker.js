// Runs SQLite on a separate Bun worker so synchronous queries never block the
// main event loop on low-end hosts. Communicates via postMessage (one query at
// a time - the caller-side LRU absorbs repeats, so message volume stays tiny).
import { Database } from 'bun:sqlite';

let db = null;
const stmtCache = new Map();

function _prepare(sql) {
    let cached = stmtCache.get(sql);
    if (!cached) {
        const q = db.query(sql);
        cached = {
            get: (...params) => q.get(...params),
            all: (...params) => q.all(...params),
            run: (...params) => q.run(...params),
        };
        stmtCache.set(sql, cached);
    }
    return cached;
}

function applyPragmas() {
    const pragmas = [
        'PRAGMA journal_mode = WAL;',
        'PRAGMA synchronous = NORMAL;',
        'PRAGMA temp_store = MEMORY;',
        'PRAGMA cache_size = -32000;',
        'PRAGMA mmap_size = 33554432;',
        'PRAGMA busy_timeout = 5000;'
    ];
    for (const pragma of pragmas) {
        try { db.run(pragma); } catch (e) { /* ignore */ }
    }
}

function init(dbPath) {
    db = new Database(dbPath, { create: true });
    applyPragmas();

    db.run(`
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
    db.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_folder_mime ON lyrics_cache(folder_id, mime_type)`);
    db.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_isrc ON lyrics_cache(isrc)`);
    db.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_platform_id ON lyrics_cache(platform_id)`);
    db.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_exact ON lyrics_cache(folder_id, mime_type, isrc, platform_id)`);
    db.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_folder_mime_isrc ON lyrics_cache(folder_id, mime_type, isrc)`);
    db.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_folder_mime_platform_id ON lyrics_cache(folder_id, mime_type, platform_id)`);
    db.run(`CREATE INDEX IF NOT EXISTS idx_lyrics_cache_updated_at ON lyrics_cache(updated_at)`);
}

function findExact(folderIDs, mimeType, songISRC, songPlatformId) {
    if (!folderIDs || folderIDs.length === 0) return null;
    if (!songISRC && !songPlatformId) return null;

    const foldersPlaceholder = folderIDs.map(() => '?').join(',');

    if (songISRC) {
        const row = _prepare(`
            SELECT id, gdrive_id, file_name, folder_id, mime_type, title, artist, album, duration, isrc, platform_id
            FROM lyrics_cache
            WHERE folder_id IN (${foldersPlaceholder}) AND mime_type = ? AND isrc = ?
            LIMIT 1
        `).get(...folderIDs, mimeType, songISRC);
        if (row) return row;
    }

    if (songPlatformId) {
        const row = _prepare(`
            SELECT id, gdrive_id, file_name, folder_id, mime_type, title, artist, album, duration, isrc, platform_id
            FROM lyrics_cache
            WHERE folder_id IN (${foldersPlaceholder}) AND mime_type = ? AND platform_id = ?
            LIMIT 1
        `).get(...folderIDs, mimeType, songPlatformId);
        if (row) return row;
    }

    return null;
}

function findExisting(folderIDs, mimeType, keywords) {
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

    return _prepare(query).all(...params);
}

function saveRow(row) {
    const now = Date.now();
    _prepare(`
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
    `).run(
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
    );
}

function getRowById(id) {
    return _prepare('SELECT * FROM lyrics_cache WHERE id = ?').get(id) || null;
}

function updateRowContent(id, content) {
    _prepare('UPDATE lyrics_cache SET content = ?, updated_at = ? WHERE id = ?').run(content, Date.now(), id);
}

self.onmessage = (e) => {
    const { id, method, args } = e.data;
    try {
        let value;
        switch (method) {
            case 'init':
                init(args[0]);
                value = true;
                break;
            case 'findExact':
                value = findExact(...args);
                break;
            case 'findExisting':
                value = findExisting(...args);
                break;
            case 'saveRow':
                saveRow(args[0]);
                value = true;
                break;
            case 'getRowById':
                value = getRowById(args[0]);
                break;
            case 'updateRowContent':
                updateRowContent(...args);
                value = true;
                break;
            default:
                throw new Error(`[DB] Unknown worker method: ${method}`);
        }
        self.postMessage({ id, ok: true, value });
    } catch (err) {
        self.postMessage({ id, ok: false, error: err?.message || String(err) });
    }
};