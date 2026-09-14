import http from 'node:http';
import { lookup } from 'node:dns/promises';
import { pathToFileURL } from 'node:url';
import { Agent } from 'undici';

const MAX_REDIRECTS = 5;
const MAX_CACHE_ENTRY_SIZE = 2 * 1024 * 1024;
const MAX_CACHE_SIZE = 32 * 1024 * 1024;
const CACHE_TTL_MS = 24 * 60 * 60 * 1000;
const AGENT_TTL_MS = 5 * 60 * 1000;
const MAX_PINNED_AGENTS = 64;
const SENSITIVE_HEADERS = new Set(['authorization', 'cookie', 'proxy-authorization', 'x-proxy-token']);
const UPSTREAM_CREDENTIAL_HEADERS = ['authorization', 'cookie', 'proxy-authorization'];
const HOP_BY_HOP_HEADERS = new Set([
  'connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization',
  'te', 'trailer', 'transfer-encoding', 'upgrade',
]);

const cache = new Map();
let cacheSize = 0;
const pinnedAgents = new Map();

function allowedHosts(value = '') {
  return new Set(value.split(',').map(host => host.trim().toLowerCase().replace(/\.$/, '')).filter(Boolean));
}

function isForbiddenIpLiteral(hostname) {
  const host = hostname.replace(/^\[|\]$/g, '').toLowerCase();
  if (host.includes(':')) {
    if (host === '::' || host === '::1') return true;
    const embeddedIpv4 = host.match(/(\d+\.\d+\.\d+\.\d+)$/);
    const mappedHex = host.match(/^(?:::ffff:|::)([\da-f]{1,4}):([\da-f]{1,4})$/);
    if (mappedHex) {
      const value = (parseInt(mappedHex[1], 16) * 0x10000) + parseInt(mappedHex[2], 16);
      return isForbiddenIpLiteral(`${value >>> 24}.${(value >>> 16) & 255}.${(value >>> 8) & 255}.${value & 255}`);
    }
    return (embeddedIpv4 && isForbiddenIpLiteral(embeddedIpv4[1])) ||
      /^(?:f[cd]|fe[89ab]|ff)/.test(host);
  }

  if (!/^\d+\.\d+\.\d+\.\d+$/.test(host)) return false;
  const octets = host.split('.').map(Number);
  if (octets.length !== 4 || octets.some(n => !Number.isInteger(n) || n < 0 || n > 255)) return true;
  const [a, b] = octets;
  return a === 0 || a === 10 || a === 127 || a >= 224 ||
    (a === 100 && b >= 64 && b <= 127) || (a === 169 && b === 254) ||
    (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 0) ||
    (a === 192 && b === 168) || (a === 198 && (b === 18 || b === 19));
}

export function validateTarget(rawTarget, allowlistValue = process.env.PROXY_ALLOWED_HOSTS || '') {
  let target;
  try {
    target = new URL(rawTarget);
  } catch {
    throw new Error('Invalid target URL');
  }

  const hostname = target.hostname.toLowerCase().replace(/\.$/, '');
  if (!['http:', 'https:'].includes(target.protocol)) throw new Error('Target protocol must be HTTP or HTTPS');
  if (target.username || target.password) throw new Error('Target URL credentials are not allowed');
  if (!hostname || !allowedHosts(allowlistValue).has(hostname)) throw new Error('Target hostname is not allowed');
  if (hostname === 'localhost' || hostname.endsWith('.localhost') || isForbiddenIpLiteral(hostname)) {
    throw new Error('Local and private target addresses are not allowed');
  }
  target.hostname = hostname;
  return target;
}

export function validateResolvedAddresses(addresses) {
  if (!addresses.length || addresses.some(({ address }) => isForbiddenIpLiteral(address))) {
    throw new Error('Target hostname resolves to a local or private address');
  }
}

async function dispatcherFor(target) {
  if (/^[\d.]+$/.test(target.hostname) || target.hostname.includes(':')) return undefined;
  const addresses = await lookup(target.hostname, { all: true, verbatim: true });
  validateResolvedAddresses(addresses);
  const now = Date.now();
  for (const [key, entry] of pinnedAgents) {
    if (entry.expiresAt <= now) {
      pinnedAgents.delete(key);
      entry.agent.close().catch(() => {});
    }
  }

  const key = `${target.protocol}//${target.host}:${addresses.map(value => `${value.address}/${value.family}`).join(',')}`;
  if (!pinnedAgents.has(key)) {
    while (pinnedAgents.size >= MAX_PINNED_AGENTS) {
      const oldestKey = pinnedAgents.keys().next().value;
      const oldest = pinnedAgents.get(oldestKey);
      pinnedAgents.delete(oldestKey);
      oldest.agent.close().catch(() => {});
    }
    const agent = new Agent({
      connect: {
        lookup: (_hostname, options, callback) => {
          if (options?.all) callback(null, addresses);
          else callback(null, addresses[0].address, addresses[0].family);
        },
      },
    });
    pinnedAgents.set(key, { agent, expiresAt: now + AGENT_TTL_MS });
  }
  const entry = pinnedAgents.get(key);
  pinnedAgents.delete(key);
  pinnedAgents.set(key, entry);
  return entry.agent;
}

function filteredRequestHeaders(headers) {
  const result = {};
  const connectionHeaders = new Set(String(headers.connection || '').split(',').map(value => value.trim().toLowerCase()));
  for (const [key, value] of Object.entries(headers || {})) {
    const name = key.toLowerCase();
    if (name === 'host' || name === 'content-length' || name === 'accept-encoding' ||
        SENSITIVE_HEADERS.has(name) || HOP_BY_HOP_HEADERS.has(name) || connectionHeaders.has(name)) continue;
    result[name] = value;
  }
  result['accept-encoding'] = 'identity';
  return result;
}

function filteredResponseHeaders(headers) {
  const result = {};
  const connectionHeaders = new Set(String(headers.get('connection') || '').split(',').map(value => value.trim().toLowerCase()));
  for (const [key, value] of headers.entries()) {
    const name = key.toLowerCase();
    if (name === 'content-length' || name === 'content-encoding' || HOP_BY_HOP_HEADERS.has(name) || connectionHeaders.has(name)) continue;
    result[name] = value;
  }
  return result;
}

async function fetchValidated(target, options, allowlistValue) {
  let current = validateTarget(target, allowlistValue);
  for (let redirects = 0; ; redirects++) {
    const dispatcher = await dispatcherFor(current);
    const response = await fetch(current, { ...options, dispatcher, redirect: 'manual' });
    if (![301, 302, 303, 307, 308].includes(response.status)) return response;
    if (redirects >= MAX_REDIRECTS) throw new Error('Too many redirects');
    const location = response.headers.get('location');
    if (!location) return response;
    if (!['GET', 'HEAD'].includes(options.method)) throw new Error('Redirects for requests with bodies are not supported');
    await response.body?.cancel();
    current = validateTarget(new URL(location, current).href, allowlistValue);
  }
}

function getCached(key) {
  const entry = cache.get(key);
  if (!entry) return undefined;
  if (entry.expires <= Date.now()) {
    cache.delete(key);
    cacheSize -= entry.body.length;
    return undefined;
  }
  cache.delete(key);
  cache.set(key, entry);
  return entry;
}

function setCached(key, entry) {
  const previous = cache.get(key);
  if (previous) cacheSize -= previous.body.length;
  cache.delete(key);
  while (cacheSize + entry.body.length > MAX_CACHE_SIZE && cache.size) {
    const oldestKey = cache.keys().next().value;
    const oldest = cache.get(oldestKey);
    cache.delete(oldestKey);
    cacheSize -= oldest.body.length;
  }
  cache.set(key, entry);
  cacheSize += entry.body.length;
}

function isCacheable(response) {
  const cacheControl = response.headers.get('cache-control') || '';
  const vary = (response.headers.get('vary') || '').toLowerCase();
  return response.ok && !response.headers.has('set-cookie') &&
    !/(?:^|,)\s*(?:no-store|private)(?:\s|=|,|$)/i.test(cacheControl) &&
    (!vary || vary.split(',').every(name => ['accept', 'accept-language', 'accept-encoding'].includes(name.trim())));
}

export function createProxyServer() {
  return http.createServer(async (req, res) => {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 15000);
    try {
      if (!process.env.PROXY_ACCESS_TOKEN) {
        res.writeHead(503);
        return res.end('Proxy access token is not configured');
      }
      if (req.headers['x-proxy-token'] !== process.env.PROXY_ACCESS_TOKEN) {
        res.writeHead(401);
        return res.end('Unauthorized');
      }
      const requestUrl = new URL(req.url, 'http://proxy.invalid');
      const rawTarget = requestUrl.searchParams.get('url');
      if (!rawTarget) {
        res.writeHead(400);
        return res.end('Missing ?url=');
      }

      const target = validateTarget(rawTarget);
      const method = (req.method || 'GET').toUpperCase();
      const anonymous = !UPSTREAM_CREDENTIAL_HEADERS.some(name => req.headers[name] != null);
      const cacheKey = `${target.href}\naccept:${req.headers.accept || ''}\naccept-language:${req.headers['accept-language'] || ''}`;
      if (method === 'GET' && anonymous) {
        const cached = getCached(cacheKey);
        if (cached) {
          res.writeHead(cached.status, { ...cached.headers, 'x-cache': 'HIT' });
          return res.end(cached.body);
        }
      }

      const requestBody = method !== 'GET' && method !== 'HEAD' ? req : undefined;
      const proxyRes = await fetchValidated(target, {
        method,
        headers: filteredRequestHeaders(req.headers),
        body: requestBody,
        duplex: requestBody ? 'half' : undefined,
        signal: controller.signal,
      }, process.env.PROXY_ALLOWED_HOSTS || '');
      const headers = filteredResponseHeaders(proxyRes.headers);
      headers['x-cache'] = 'MISS';
      headers['access-control-allow-origin'] = '*';
      res.writeHead(proxyRes.status, headers);
      if (!proxyRes.body) return res.end();

      const shouldCache = method === 'GET' && anonymous && isCacheable(proxyRes);
      const chunks = [];
      let bufferedSize = 0;
      let tooLarge = false;
      const reader = proxyRes.body.getReader();
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        res.write(value);
        if (shouldCache && !tooLarge) {
          bufferedSize += value.byteLength;
          if (bufferedSize <= MAX_CACHE_ENTRY_SIZE) chunks.push(Buffer.from(value));
          else {
            tooLarge = true;
            chunks.length = 0;
          }
        }
      }
      res.end();
      if (shouldCache && !tooLarge) {
        const body = Buffer.concat(chunks, bufferedSize);
        setCached(cacheKey, { status: proxyRes.status, headers, body, expires: Date.now() + CACHE_TTL_MS });
      }
    } catch (error) {
      if (!res.headersSent) res.writeHead(error.message?.includes('target') || error.message?.includes('allowed') ? 400 : 502);
      if (!res.writableEnded) res.end(`Proxy error: ${error.message}`);
    } finally {
      clearTimeout(timeout);
    }
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const PORT = process.env.PORT || 6464;
  createProxyServer().listen(PORT, () => console.log(`Running on port ${PORT}`));
}
