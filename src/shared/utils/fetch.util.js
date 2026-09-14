import { SERVER } from "../config.js";
import { logger } from "./logger.util.js";
import { AsyncLocalStorage } from 'node:async_hooks';

const fetchContext = new AsyncLocalStorage();
const DEFAULT_TIMEOUT_MS = 15000;

export function getActiveFetchSignal() {
  return fetchContext.getStore()?.signal;
}

export function runWithFetchSignal(signal, callback) {
  const activeSignal = getActiveFetchSignal();
  const combined = combineSignals([activeSignal, signal]);
  return fetchContext.run({ signal: combined.signal }, () => {
    try {
      return Promise.resolve(callback()).finally(combined.cleanup);
    } catch (error) {
      combined.cleanup();
      throw error;
    }
  });
}

export function combineSignals(signals) {
  const validSignals = signals.filter(Boolean);
  if (validSignals.length === 0) return { signal: undefined, cleanup: () => {} };
  if (validSignals.length === 1) return { signal: validSignals[0], cleanup: () => {} };

  const controller = new AbortController();
  let cleanedUp = false;

  const cleanup = () => {
    if (cleanedUp) return;
    cleanedUp = true;
    for (const signal of validSignals) {
      signal.removeEventListener('abort', onAbort);
    }
  };

  const onAbort = () => {
    controller.abort();
    cleanup();
  };

  for (const signal of validSignals) {
    if (signal.aborted) {
      controller.abort();
      cleanup();
      break;
    }
    signal.addEventListener('abort', onAbort, { once: true });
  }

  return {
    signal: controller.signal,
    cleanup,
  };
}

async function fetchWithDeadline(url, options = {}) {
  const timeout = Number.isFinite(options.timeout) ? options.timeout : DEFAULT_TIMEOUT_MS;
  const timeoutController = new AbortController();
  const timeoutId = setTimeout(() => timeoutController.abort(), timeout);
  const { signal, cleanup } = combineSignals([options.signal, getActiveFetchSignal(), timeoutController.signal]);
  const { timeout: _timeout, ...fetchOptions } = options;

  const headers = new Headers(fetchOptions.headers || {});
  if (!headers.has('Connection')) {
    headers.set('Connection', 'keep-alive');
  }

  try {
    return await fetch(url, { ...fetchOptions, headers, signal });
  } finally {
    clearTimeout(timeoutId);
    cleanup();
  }
}

function addProxyAuthentication(options, proxied) {
  const headers = new Headers(options.headers);
  if (proxied && SERVER.PROXY.ACCESS_TOKEN) headers.set('x-proxy-token', SERVER.PROXY.ACCESS_TOKEN);
  return { ...options, headers };
}

/**
 * Determines if a URL should be proxied
 * @param {string} url - The URL to check
 * @returns {boolean} - Whether the URL should be proxied
 */
function shouldProxy(url) {
  try {
    const urlObj = new URL(url);
    // Don't proxy localhost or internal URLs
    if (urlObj.hostname === 'localhost' || urlObj.hostname === '127.0.0.1' || urlObj.hostname.endsWith('.local')) {
      return false;
    }
    return true;
  } catch {
    return false;
  }
}

/**
 * Applies proxy to a URL if proxy is enabled
 * @param {string} url - The original URL
 * @returns {string} - The proxied URL or original URL
 */
function applyProxy(url) {
  if (!SERVER.PROXY.ENABLED || !SERVER.PROXY.URLS || SERVER.PROXY.URLS.length === 0) {
    return url;
  }

  if (!shouldProxy(url)) {
    return url;
  }

  // Use a random proxy URL from the available list
  const proxyUrl = SERVER.PROXY.URLS[Math.floor(Math.random() * SERVER.PROXY.URLS.length)];
  const proxiedUrl = `${proxyUrl}${encodeURIComponent(url)}`;

  return proxiedUrl;
}

/**
 * Enhanced fetch with proxy support
 * @param {string} url - The URL to fetch
 * @param {object} options - Fetch options
 * @returns {Promise<Response>} - The fetch response
 */
export async function fetchWithProxy(url, options = {}) {
  try {
    const proxiedUrl = applyProxy(url);
    const response = await fetchWithDeadline(proxiedUrl, addProxyAuthentication(options, proxiedUrl !== url));
    return response;
  } catch (error) {
    logger.error(`[Fetch] Error fetching ${url}: ${error.message}`);
    throw error;
  }
}

/**
 * Selective proxy fetch - only proxies if explicitly enabled for that request
 * Useful for APIs that don't work well with proxies
 * @param {string} url - The URL to fetch
 * @param {object} options - Fetch options
 * @param {boolean} useProxy - Whether to use proxy for this request
 * @returns {Promise<Response>} - The fetch response
 */
export async function fetchSelective(url, options = {}, useProxy = true) {
  try {
    const finalUrl = useProxy ? applyProxy(url) : url;
    const response = await fetchWithDeadline(finalUrl, addProxyAuthentication(options, finalUrl !== url));
    return response;
  } catch (error) {
    logger.error(`[Fetch] Error fetching ${url}: ${error.message}`);
    throw error;
  }
}

/**
 * Get the proxied URL without making the request
 * Useful for debugging or conditions where you need the URL first
 * @param {string} url - The original URL
 * @returns {string} - The proxied URL or original URL
 */
export function getProxiedUrl(url) {
  return applyProxy(url);
}

/**
 * Check if proxy is currently enabled
 * @returns {boolean} - Whether proxy is enabled
 */
export function isProxyEnabled() {
  return SERVER.PROXY.ENABLED && SERVER.PROXY.URLS && SERVER.PROXY.URLS.length > 0;
}

export default fetchWithProxy;
