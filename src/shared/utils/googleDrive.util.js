import { AUTH_KEY, GDRIVE, CACHE_CONFIG } from "../config.js";
import { combineSignals } from './fetch.util.js';

const FETCH_TIMEOUT_MS = 15_000;
const TOKEN_EXPIRY_SKEW_MS = 60_000;

// Rate limiting & Exponential backoff settings
const MAX_RETRIES = 3;
const INITIAL_RETRY_DELAY_MS = 500;
const MAX_RETRY_DELAY_MS = 5000;
const MAX_TOTAL_RETRY_MS = 12000;
const DEFAULT_CONCURRENCY = 15;

// Circuit breaker settings
const CIRCUIT_BREAKER_THRESHOLD = 5;    // Open circuit after 5 consecutive rate-limit errors
const CIRCUIT_BREAKER_COOLDOWN_MS = 60_000; // 60s cooldown before retrying GDrive

// --- High-efficiency bounded LRU caches ---
class SimpleLRU {
  constructor(maxSize = 500) {
    this.maxSize = maxSize;
    this.cache = new Map();
  }

  get(key) {
    const item = this.cache.get(key);
    if (!item) return null;
    if (item.expiresAt && Date.now() > item.expiresAt) {
      this.cache.delete(key);
      return null;
    }
    // Refresh LRU order
    this.cache.delete(key);
    this.cache.set(key, item);
    return item.value;
  }

  set(key, value, ttlMs = 15 * 60 * 1000) {
    if (this.cache.has(key)) {
      this.cache.delete(key);
    } else if (this.cache.size >= this.maxSize) {
      const oldestKey = this.cache.keys().next().value;
      if (oldestKey !== undefined) this.cache.delete(oldestKey);
    }
    this.cache.set(key, { value, expiresAt: ttlMs > 0 ? Date.now() + ttlMs : 0 });
  }

  delete(key) {
    return this.cache.delete(key);
  }

  clear() {
    this.cache.clear();
  }

  get size() {
    return this.cache.size;
  }
}

const searchCache = new SimpleLRU(1000);
const fileContentCache = new SimpleLRU(500);
const listCache = new SimpleLRU(50);

const CACHE_TTL_MS = 15 * 60 * 1000;
const NEGATIVE_CACHE_TTL_MS = 5 * 1000; // 5s for empty/missing search results

export function invalidateSearchCache() {
  // During a rate-limit storm, keep serving stale-but-valid cached results
  // instead of clearing the caches and forcing GDrive re-reads.
  if (circuitOpen) return;
  searchCache.clear();
  listCache.clear();
}

export function invalidateAllCaches() {
  searchCache.clear();
  fileContentCache.clear();
  listCache.clear();
}

class RequestQueue {
  constructor(maxConcurrency = DEFAULT_CONCURRENCY) {
    this.maxConcurrency = maxConcurrency;
    this.activeCount = 0;
    this.queue = [];
  }

  setConcurrency(limit) {
    if (Number.isFinite(limit) && limit > 0) {
      this.maxConcurrency = limit;
      this._drain();
    }
  }

  _drain() {
    while (this.activeCount < this.maxConcurrency && this.queue.length > 0) {
      const next = this.queue.shift();
      next();
    }
  }

  async run(fn) {
    if (this.activeCount >= this.maxConcurrency) {
      await new Promise((resolve) => this.queue.push(resolve));
    }
    this.activeCount++;
    try {
      return await fn();
    } finally {
      this.activeCount--;
      this._drain();
    }
  }
}

const gdriveQueue = new RequestQueue(DEFAULT_CONCURRENCY);
const inFlightRequests = new Map();
const uploadLocks = new Map();

// Circuit breaker state
let circuitOpen = false;
let circuitOpenUntil = 0;
let consecutiveRateLimitErrors = 0;
let rateLimitCooldownUntil = 0;

/**
 * Global cooldown across all accounts. When a rate limit is hit, enforce a short
 * global pause so we don't fan out more requests to Google simultaneously.
 */
function isInRateLimitCooldown() {
  return rateLimitCooldownUntil > Date.now();
}

function enterRateLimitCooldown(ms = 1500) {
  rateLimitCooldownUntil = Date.now() + ms;
}

/**
 * Circuit breaker: once GDrive trips it, all reads/writes short-circuit
 * (return cached/DB data or a typed error) so we stop hammering the API
 * and let the server keep serving other sources.
 */
function isCircuitOpen() {
  if (!circuitOpen) return false;
  if (Date.now() >= circuitOpenUntil) {
    circuitOpen = false;
    consecutiveRateLimitErrors = 0;
    console.warn('[GDrive] Circuit breaker closed, resuming GDrive requests.');
  }
  return circuitOpen;
}

function tripCircuit() {
  if (circuitOpen) return;
  circuitOpen = true;
  circuitOpenUntil = Date.now() + CIRCUIT_BREAKER_COOLDOWN_MS;
  consecutiveRateLimitErrors = 0;
  console.warn(`[GDrive] Circuit breaker OPENED for ${CIRCUIT_BREAKER_COOLDOWN_MS / 1000}s due to repeated rate limiting. Serving from local cache only.`);
}

function recordRateLimitError() {
  consecutiveRateLimitErrors++;
  enterRateLimitCooldown();
  if (consecutiveRateLimitErrors >= CIRCUIT_BREAKER_THRESHOLD) {
    tripCircuit();
  }
}

function recordRateLimitSuccess() {
  if (consecutiveRateLimitErrors > 0) {
    consecutiveRateLimitErrors = Math.max(0, consecutiveRateLimitErrors - 1);
  }
}

class GDriveCircuitOpenError extends Error {
  constructor() {
    super('[GDrive] Circuit breaker is open. Skipping GDrive request.');
    this.name = 'GDriveCircuitOpenError';
    this.isCircuitOpen = true;
  }
}

function handleApiError(body, response) {
  const parsedBody = typeof body === "string" ? (() => {
    try { return JSON.parse(body); } catch { return body; }
  })() : body;
  const apiError = parsedBody?.error;
  const error = new Error(apiError?.message || response?.statusText || "Unknown Google API error.");
  error.status = response?.status ?? apiError?.code;
  error.reason = apiError?.errors?.[0]?.reason || response?.statusText;
  error.body = parsedBody;
  return error;
}

async function fetchWithTimeout(url, options = {}) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);
  const { signal, cleanup } = combineSignals([options.signal, controller.signal]);
  try {
    return await fetch(url, { ...options, signal });
  } finally {
    clearTimeout(timeout);
    cleanup();
  }
}

/** Error codes returned by the Drive API that indicate a folder/storage quota is full. */
const STORAGE_QUOTA_ERROR_CODES = [
  'storageQuotaExceeded',
  'teamDriveFileLimitExceeded',
  'folderItemLimitExceeded',
  'numChildrenExceeded',
  'appNotAuthorizedToFile',
];

/** Error codes returned by the Drive API that indicate API rate limits. */
const RATE_LIMIT_ERROR_CODES = [
  'userRateLimitExceeded',
  'rateLimitExceeded',
  'quotaExceeded',
  'rateLimitExceededUnreg',
  'dailyLimitExceeded',
];

export function isStorageQuotaFullError(error) {
  const msg = (error?.message || error?.reason || '').toLowerCase();
  const bodyStr = JSON.stringify(error?.body || '').toLowerCase();
  return STORAGE_QUOTA_ERROR_CODES.some(code => msg.includes(code.toLowerCase()) || bodyStr.includes(code.toLowerCase()))
    || msg.includes('storage quota')
    || msg.includes('drive full')
    || msg.includes('number of children')
    || msg.includes('children (files and folders)')
    || msg.includes('limit for this folder')
    || msg.includes('folder\'s number of children');
}

export function isRateLimitError(error, responseStatus) {
  if (isStorageQuotaFullError(error)) return false;
  if (responseStatus === 429) return true;
  if (responseStatus === 403 || error?.status === 403) {
    const msg = (error?.message || error?.reason || '').toLowerCase();
    const bodyStr = JSON.stringify(error?.body || '').toLowerCase();
    return RATE_LIMIT_ERROR_CODES.some(code => msg.includes(code.toLowerCase()) || bodyStr.includes(code.toLowerCase()))
      || msg.includes('user rate limit')
      || msg.includes('rate limit')
      || (msg.includes('quota exceeded') && !msg.includes('folder'))
      || msg.includes('daily limit');
  }
  return false;
}

export function isQuotaError(error) {
  return isStorageQuotaFullError(error) || isRateLimitError(error, error?.status);
}

export function isCircuitBreakerOpen() {
  return isCircuitOpen();
}

async function fetchWithRetry(fetchFn, options = {}) {
  const { maxRetries = MAX_RETRIES, initialDelayMs = INITIAL_RETRY_DELAY_MS } = options;
  let attempt = 0;
  const startedAt = Date.now();

  while (true) {
    try {
      const response = await fetchFn();

      if (response.status === 429 || response.status === 403 || (response.status >= 500 && response.status < 600)) {
        const bodyText = await response.clone().text().catch(() => '');
        let parsedBody;
        try { parsedBody = JSON.parse(bodyText); } catch { parsedBody = bodyText; }
        const errObj = handleApiError(parsedBody, response);

        if (!isStorageQuotaFullError(errObj) && (isRateLimitError(errObj, response.status) || response.status >= 500)) {
          if (isRateLimitError(errObj, response.status)) {
            recordRateLimitError();
          } else {
            recordRateLimitSuccess();
          }

          if (attempt < maxRetries && Date.now() - startedAt < MAX_TOTAL_RETRY_MS) {
            attempt++;
            let delayMs = Math.min(MAX_RETRY_DELAY_MS, initialDelayMs * Math.pow(2, attempt - 1));

            const retryAfter = response.headers.get('Retry-After');
            if (retryAfter) {
              const parsedRetry = parseInt(retryAfter, 10);
              if (!isNaN(parsedRetry)) {
                delayMs = parsedRetry * 1000;
              }
            } else {
              const jitter = Math.random() * 0.4 * delayMs - 0.2 * delayMs;
              delayMs = Math.max(100, Math.floor(delayMs + jitter));
            }

            delayMs = Math.min(delayMs, MAX_RETRY_DELAY_MS);
            console.warn(`[GDrive RateLimit] HTTP ${response.status} (${errObj.reason || errObj.message}). Retrying attempt ${attempt}/${maxRetries} in ${delayMs}ms...`);
            await new Promise(r => setTimeout(r, delayMs));
            continue;
          }

          console.warn(`[GDrive] Rate limit retries exhausted after ${Date.now() - startedAt}ms. Failing fast.`);
          throw errObj;
        }

        if (response.status >= 500 && response.status < 600) recordRateLimitSuccess();
      }
      recordRateLimitSuccess();
      return response;
    } catch (err) {
      if (err.name === 'AbortError' || err.name === 'TimeoutError') {
        throw err;
      }
      if (attempt < maxRetries && Date.now() - startedAt < MAX_TOTAL_RETRY_MS && (err.name === 'FetchError' || err.code === 'ECONNRESET' || err.code === 'ETIMEDOUT')) {
        attempt++;
        const delayMs = Math.min(MAX_RETRY_DELAY_MS, initialDelayMs * Math.pow(2, attempt - 1));
        console.warn(`[GDrive NetworkError] ${err.message}. Retrying attempt ${attempt}/${maxRetries} in ${delayMs}ms...`);
        await new Promise(r => setTimeout(r, delayMs));
        continue;
      }
      throw err;
    }
  }
}

export default class GoogleDrive {
  constructor(authConfig = AUTH_KEY) {
    this.authConfig = authConfig;
    this.accountIndex = 0;
    // Map of accountIndex -> { accessToken, expires }
    this.tokenMap = new Map();
    // Map of accountIndex -> Promise
    this.refreshPromises = new Map();
  }

  get accounts() {
    if (Array.isArray(this.authConfig.ACCOUNTS) && this.authConfig.ACCOUNTS.length > 0) {
      return this.authConfig.ACCOUNTS;
    }
    return [this.authConfig];
  }

  get currentAccount() {
    const list = this.accounts;
    return list[this.accountIndex % list.length];
  }

  rotateAccount() {
    const list = this.accounts;
    if (list.length > 1) {
      this.accountIndex = (this.accountIndex + 1) % list.length;
      console.warn(`[GDrive] Switched auth account to index ${this.accountIndex} (${this.currentAccount.CLIENT_ID?.slice(0, 12)}...)`);
      return true;
    }
    return false;
  }

  async authenticate(forceRefresh = false) {
    if (isCircuitOpen()) {
      throw new GDriveCircuitOpenError();
    }
    if (!CACHE_CONFIG.GDRIVE_ENABLED) {
      throw new Error('[GDrive] Google Drive is disabled in configuration.');
    }

    const idx = this.accountIndex % this.accounts.length;
    const cachedToken = this.tokenMap.get(idx);

    if (!forceRefresh && cachedToken && cachedToken.expires - TOKEN_EXPIRY_SKEW_MS > Date.now()) {
      return cachedToken.accessToken;
    }

    if (!this.refreshPromises.has(idx)) {
      const refreshPromise = (async () => {
        const account = this.currentAccount;
        const response = await fetchWithRetry(() => fetchWithTimeout("https://www.googleapis.com/oauth2/v4/token", {
          method: "POST",
          headers: { "Content-Type": "application/x-www-form-urlencoded" },
          body: new URLSearchParams({
            client_id: account.CLIENT_ID,
            client_secret: account.CLIENT_SECRET,
            refresh_token: account.REFRESH_TOKEN,
            grant_type: "refresh_token",
          }),
        }));

        const body = await response.text();
        let data;
        try { data = JSON.parse(body); } catch { data = body; }

        if (response.ok) {
          const accessToken = data.access_token;
          const expires = Date.now() + (Number(data.expires_in) || 3600) * 1000;
          this.tokenMap.set(idx, { accessToken, expires });
          console.debug(`[GDrive] AccessToken updated for account #${idx}`);
          return accessToken;
        } else {
          throw handleApiError(data, response);
        }
      })().finally(() => {
        this.refreshPromises.delete(idx);
      });

      this.refreshPromises.set(idx, refreshPromise);
    }

    return this.refreshPromises.get(idx);
  }

  /**
   * Executes an operation with automatic account rotation if persistent rate limiting occurs.
   */
  async withAutoRotation(fn, maxRotations = 0) {
    const allowedRotations = maxRotations > 0 ? maxRotations : Math.max(1, this.accounts.length);
    let rotations = 0;

    while (true) {
      if (isCircuitOpen()) throw new GDriveCircuitOpenError();
      try {
        return await fn();
      } catch (err) {
        if (err?.isCircuitOpen) throw err;
        if (isRateLimitError(err, err?.status) && rotations < allowedRotations && !isInRateLimitCooldown()) {
          if (this.rotateAccount()) {
            rotations++;
            console.warn(`[GDrive] Auto-rotated account due to rate limit (rotation ${rotations}/${allowedRotations})`);
            continue;
          }
        }
        throw err;
      }
    }
  }

  async request(url, method = "GET", body = null, options = {}) {
    const isRead = method === "GET";
    const cacheKey = isRead ? `REQ:${url}` : null;

    if (isRead && !options.skipCache) {
      const cached = fileContentCache.get(cacheKey);
      if (cached !== null) return cached;
    }

    if (cacheKey && inFlightRequests.has(cacheKey)) {
      return inFlightRequests.get(cacheKey);
    }

    const executeRequest = async () => {
      return this.withAutoRotation(async () => {
        const token = await this.authenticate();
        const headers = { Authorization: `Bearer ${token}`, "Content-Type": "application/json" };
        const reqOptions = { method, headers, body };

        const response = await fetchWithRetry(() => fetchWithTimeout(url, reqOptions));
        const data = await response.text();
        if (!response.ok) throw handleApiError(data, response);
        return data;
      });
    };

    const promise = isRead
      ? (async () => {
          const data = await executeRequest();
          if (!options.skipCache) {
            fileContentCache.set(cacheKey, data, CACHE_TTL_MS);
          }
          return data;
        })()
      : gdriveQueue.run(executeRequest);

    if (cacheKey) {
      inFlightRequests.set(cacheKey, promise);
      promise.catch(() => {});
      promise.finally(() => inFlightRequests.delete(cacheKey)).catch(() => {});
    }

    return promise;
  }

  async fetchFile(fileId, options = {}) {
    if (typeof fileId === 'string' && fileId.startsWith('db:')) {
      const { db } = await import('./db.util.js');
      const row = await db.getRowById(fileId);
      return row ? row.content : null;
    }
    if (!CACHE_CONFIG.GDRIVE_ENABLED) {
      throw new Error('[GDrive] Google Drive is disabled, cannot fetch file.');
    }
    return this.request(`${GDRIVE.API_URL}${fileId}?alt=media`, "GET", null, options);
  }

  async updateFile(fileId, data, options = {}) {
    if (typeof fileId === 'string' && fileId.startsWith('db:')) {
      const { db } = await import('./db.util.js');
      const dbRow = await db.getRowById(fileId);

      if (CACHE_CONFIG.DB_ENABLED) {
        await db.updateRowContent(fileId, data);
      }

      if (CACHE_CONFIG.GDRIVE_ENABLED && dbRow && dbRow.gdrive_id) {
        const res = await this.request(`${GDRIVE.API_URL_UPDATE}${dbRow.gdrive_id}?uploadType=media`, "PUT", data, options);
        if (!options.skipInvalidate) invalidateSearchCache();
        return res;
      }

      return { id: fileId };
    }

    if (CACHE_CONFIG.GDRIVE_ENABLED) {
      const res = await this.request(`${GDRIVE.API_URL_UPDATE}${fileId}?uploadType=media`, "PUT", data, options);
      if (!options.skipInvalidate) invalidateSearchCache();
      return res;
    }
    throw new Error('[GDrive] Google Drive is disabled, cannot update file.');
  }

  static async withUploadLock(key, fn) {
    const previous = uploadLocks.get(key) || Promise.resolve();
    const current = previous.catch(() => {}).then(fn);
    uploadLocks.set(key, current);
    try {
      return await current;
    } finally {
      if (uploadLocks.get(key) === current) {
        uploadLocks.delete(key);
      }
    }
  }

  async findExactFileByName(fileName, folderId = null, options = {}) {
    const escapedName = String(fileName).replace(/\\/g, '\\\\').replace(/'/g, "\\'");
    let query = `name = '${escapedName}' and trashed = false`;
    if (folderId) {
      const escapedFolder = String(folderId).replace(/\\/g, '\\\\').replace(/'/g, "\\'");
      query += ` and '${escapedFolder}' in parents`;
    }
    const result = await this.searchFiles(query, null, { pageSize: 1, ...options });
    return result?.files?.[0] || null;
  }

  async uploadFile(fileName, mimeType, fileData, folderId = null, options = {}) {
    if (isCircuitOpen()) {
      throw new GDriveCircuitOpenError();
    }
    // If not explicitly bypassed, check if exact file already exists in folderId to prevent duplicates
    if (!options.skipExistingCheck) {
      try {
        const existing = await this.findExactFileByName(fileName, folderId, options);
        if (existing) {
          console.debug(`[GDrive] File "${fileName}" already exists (${existing.id}). Updating instead of creating duplicate.`);
          return await this.updateFile(existing.id, fileData, options);
        }
      } catch (err) {
        if (isRateLimitError(err, err?.status)) {
          throw err;
        }
      }
    }

    return gdriveQueue.run(async () => {
      return this.withAutoRotation(async () => {
        const token = await this.authenticate();
        const metadata = {
          name: fileName,
          mimeType,
          parents: folderId ? [folderId] : [],
        };

        const formData = new FormData();
        formData.append("metadata", new Blob([JSON.stringify(metadata)], { type: "application/json" }));
        formData.append("file", fileData);

        const response = await fetchWithRetry(() => fetchWithTimeout("https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart", {
          method: "POST",
          headers: { Authorization: `Bearer ${token}` },
          body: formData,
        }));
        const body = await response.text();
        let data;
        try { data = JSON.parse(body); } catch { data = body; }
        if (!response.ok) throw handleApiError(data, response);
        if (!options.skipInvalidate) invalidateSearchCache();
        return data;
      });
    });
  }

  async _uploadFileWithFallbackOriginal(fileName, mimeType, fileData, folderIds = [], options = {}) {
    const folders = Array.isArray(folderIds) ? folderIds.filter(Boolean) : (folderIds ? [folderIds] : []);
    if (folders.length === 0) {
      return this.uploadFile(fileName, mimeType, fileData, null, options);
    }

    // 1. Check if file with the exact name already exists in ANY of the configured folders
    if (!options.skipExistingCheck) {
      for (const folderId of folders) {
        try {
          const existing = await this.findExactFileByName(fileName, folderId, options);
          if (existing) {
            console.debug(`[GDrive] Exact file "${fileName}" found in folder (${folderId}: ${existing.id}). Updating in place.`);
            return await this.updateFile(existing.id, fileData, options);
          }
        } catch (err) {
          if (isRateLimitError(err, err?.status)) {
            throw err;
          }
          console.warn(`[GDrive] Warning: Error checking exact file in folder ${folderId}:`, err.message);
        }
      }
    }

    // 2. Upload to folders in order, falling back on storage quota errors
    for (let i = 0; i < folders.length; i++) {
      const folderId = folders[i];
      try {
        const result = await this.uploadFile(fileName, mimeType, fileData, folderId, { ...options, skipExistingCheck: true });
        if (i > 0) {
          console.warn(`[GDrive] Uploaded to fallback folder #${i + 1} (${folderId}).`);
        }
        return result;
      } catch (err) {
        if (isStorageQuotaFullError(err) && i < folders.length - 1) {
          console.warn(`[GDrive] Folder #${i + 1} (${folderId}) is full. Trying next folder...`);
          continue;
        }
        throw err;
      }
    }

    throw new Error('[GDrive] All configured folders are full or quota exhausted.');
  }

  async uploadFileWithFallback(fileName, mimeType, fileData, folderIds = [], options = {}) {
    const lockKey = `${Array.isArray(folderIds) ? folderIds.join(',') : (folderIds || 'root')}:${fileName}`;
    return GoogleDrive.withUploadLock(lockKey, async () => {
      let gdriveResult = null;
      let dbId = null;
      let gdriveAttempted = false;

      if (CACHE_CONFIG.GDRIVE_ENABLED) {
        gdriveAttempted = true;
        try {
          gdriveResult = await this._uploadFileWithFallbackOriginal(fileName, mimeType, fileData, folderIds, options);
        } catch (err) {
          if (err?.isCircuitOpen) {
            console.warn(`[GDrive] Skipping upload of "${fileName}": circuit breaker is open. Saving to local DB only.`);
          } else if (isRateLimitError(err, err?.status)) {
            console.warn(`[GDrive] Upload of "${fileName}" rate limited: ${err.message}. Saving to local DB only.`);
          } else {
            console.warn(`[GDrive] Upload of "${fileName}" failed: ${err.message}. Saving to local DB only.`);
          }
        }
      }

      if (CACHE_CONFIG.DB_ENABLED) {
        try {
          const { FileUtils } = await import('./file.util.js');
          const parsed = FileUtils._parseFileName(fileName);
          const folderId = Array.isArray(folderIds) ? folderIds[0] : folderIds;
          const { db } = await import('./db.util.js');

          dbId = crypto.randomUUID();
          await db.saveRow({
            id: dbId,
            gdrive_id: gdriveResult?.id || null,
            file_name: fileName,
            folder_id: folderId,
            mime_type: mimeType,
            content: typeof fileData === 'string' ? fileData : JSON.stringify(fileData),
            title: parsed.title,
            artist: parsed.artist,
            album: parsed.album,
            duration: parsed.duration,
            isrc: parsed.isrc,
            platform_id: parsed.platformId
          });
        } catch (err) {
          console.error("Failed to save uploaded file to local DB:", err);
        }
      }

      if (gdriveResult) return gdriveResult;
      if (gdriveAttempted && !CACHE_CONFIG.DB_ENABLED) throw new Error('[GDrive] Upload failed and local DB is disabled.');
      return { id: `db:${dbId || crypto.randomUUID()}` };
    });
  }

  /**
   * Async generator that yields files page by page.
   * Keeps memory usage at O(1) (only 1 page in memory at a time).
   */
  async *searchFilesStream(query, options = {}) {
    if (isCircuitOpen()) {
      yield { files: [], pageToken: undefined, hasNextPage: false };
      return;
    }
    const { fields = "files(id,name,mimeType,createdTime,modifiedTime)", pageSize = 1000 } = options;
    let pageToken;

    do {
      const pageResult = await this.withAutoRotation(async () => {
        const token = await this.authenticate();
        const url = new URL("https://www.googleapis.com/drive/v3/files");
        url.searchParams.set("q", `${query} and trashed = false`);
        url.searchParams.set("fields", `nextPageToken,${fields}`);
        url.searchParams.set("pageSize", String(pageSize));
        if (pageToken) url.searchParams.set("pageToken", pageToken);

        const response = await fetchWithRetry(() => fetchWithTimeout(url, {
          headers: {
            Authorization: `Bearer ${token}`,
            'Content-Type': 'application/json'
          }
        }));

        const body = await response.text();
        let data;
        try { data = JSON.parse(body); } catch { data = body; }
        if (!response.ok) throw handleApiError(data, response);
        return data;
      });

      const pageFiles = pageResult.files || [];
      pageToken = pageResult.nextPageToken;

      yield {
        files: pageFiles,
        pageToken,
        hasNextPage: Boolean(pageToken),
      };
    } while (pageToken);
  }

  /**
   * Async generator that yields files in a folder page by page.
   */
  async *listFilesStream(folderId, options = {}) {
    if (isCircuitOpen()) {
      yield { files: [], pageToken: undefined, hasNextPage: false };
      return;
    }
    const {
      fields = "files(id,name,mimeType,createdTime,modifiedTime)",
      pageSize = 1000,
      extraQuery = ""
    } = options;

    const baseQuery = `'${String(folderId).replace(/\\/g, "\\\\").replace(/'/g, "\\'")}' in parents and trashed = false`;
    const fullQuery = extraQuery ? `${baseQuery} and (${extraQuery})` : baseQuery;

    let pageToken;
    do {
      const pageResult = await this.withAutoRotation(async () => {
        const token = await this.authenticate();
        const url = new URL("https://www.googleapis.com/drive/v3/files");
        url.searchParams.set("q", fullQuery);
        url.searchParams.set("fields", `nextPageToken,${fields}`);
        url.searchParams.set("pageSize", String(pageSize));
        if (pageToken) url.searchParams.set("pageToken", pageToken);

        const response = await fetchWithRetry(() => fetchWithTimeout(url, {
          headers: {
            Authorization: `Bearer ${token}`,
            'Content-Type': 'application/json'
          }
        }));

        const body = await response.text();
        let data;
        try { data = JSON.parse(body); } catch { data = body; }
        if (!response.ok) throw handleApiError(data, response);
        return data;
      });

      const pageFiles = pageResult.files || [];
      pageToken = pageResult.nextPageToken;

      yield {
        files: pageFiles,
        pageToken,
        hasNextPage: Boolean(pageToken),
      };
    } while (pageToken);
  }

  async searchFiles(query, onProgress = null, options = {}) {
    const cacheKey = `SEARCH:${query}`;
    if (!options.skipCache) {
      const cached = searchCache.get(cacheKey);
      if (cached) {
        if (typeof onProgress === "function") onProgress(cached.files?.length || 0, cached.files || []);
        return cached;
      }
    }

    if (isCircuitOpen()) {
      console.debug(`[GDrive] searchFiles skipped (circuit open): ${query.slice(0, 80)}...`);
      const empty = { files: [] };
      searchCache.set(cacheKey, empty, NEGATIVE_CACHE_TTL_MS);
      return empty;
    }

    if (inFlightRequests.has(cacheKey)) {
      return inFlightRequests.get(cacheKey);
    }

    const promise = (async () => {
      const files = [];
      for await (const { files: pageFiles } of this.searchFilesStream(query, options)) {
        files.push(...pageFiles);
        if (typeof onProgress === "function") onProgress(files.length, files);
      }

      const res = { files };
      if (!options.skipCache) {
        const ttl = files.length === 0 ? NEGATIVE_CACHE_TTL_MS : CACHE_TTL_MS;
        searchCache.set(cacheKey, res, ttl);
      }
      return res;
    })();

    inFlightRequests.set(cacheKey, promise);
    promise.catch(() => {});
    promise.finally(() => inFlightRequests.delete(cacheKey)).catch(() => {});
    return promise;
  }

  async listFiles(folderId, onProgress = null, options = {}) {
    const cacheKey = `LIST:${folderId}`;
    if (!options.skipCache) {
      const cached = listCache.get(cacheKey);
      if (cached) {
        if (typeof onProgress === "function") onProgress(cached.length, cached);
        return cached;
      }
    }

    if (isCircuitOpen()) {
      console.debug(`[GDrive] listFiles skipped (circuit open): ${folderId}`);
      return [];
    }

    if (inFlightRequests.has(cacheKey)) {
      return inFlightRequests.get(cacheKey);
    }

    const promise = (async () => {
      const files = [];
      for await (const { files: pageFiles } of this.listFilesStream(folderId, options)) {
        files.push(...pageFiles);
        if (typeof onProgress === "function") onProgress(files.length, files);
      }

      if (!options.skipCache) {
        listCache.set(cacheKey, files, CACHE_TTL_MS);
      }
      return files;
    })();

    inFlightRequests.set(cacheKey, promise);
    promise.catch(() => {});
    promise.finally(() => inFlightRequests.delete(cacheKey)).catch(() => {});
    return promise;
  }

  async deleteFile(fileId, options = {}) {
    return gdriveQueue.run(async () => {
      return this.withAutoRotation(async () => {
        const token = await this.authenticate();
        const response = await fetchWithRetry(() => fetchWithTimeout(
          `${GDRIVE.API_URL}${fileId}`,
          {
            method: "DELETE",
            headers: {
              Authorization: `Bearer ${token}`
            }
          }
        ));

        if (!response.ok) {
          // If 404, file is already deleted
          if (response.status === 404) return true;
          throw handleApiError(await response.text(), response);
        }

        if (!options.skipInvalidate) {
          invalidateSearchCache();
        }
        return true;
      });
    });
  }
}
