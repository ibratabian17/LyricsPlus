const CACHE_TTL_SECONDS = 24 * 60 * 60;
const MAX_REDIRECTS = 5;
const SENSITIVE_HEADERS = new Set(['authorization', 'cookie', 'proxy-authorization', 'x-proxy-token']);
const UPSTREAM_CREDENTIAL_HEADERS = ['authorization', 'cookie', 'proxy-authorization'];
const HOP_BY_HOP_HEADERS = new Set([
  'connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization',
  'te', 'trailer', 'transfer-encoding', 'upgrade',
]);

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

export function validateTarget(rawTarget, allowlistValue = '') {
  let target;
  try {
    target = new URL(rawTarget);
  } catch {
    throw new Error('Invalid target URL');
  }
  const hostname = target.hostname.toLowerCase().replace(/\.$/, '');
  const allowed = new Set(allowlistValue.split(',').map(host => host.trim().toLowerCase().replace(/\.$/, '')).filter(Boolean));
  if (!['http:', 'https:'].includes(target.protocol)) throw new Error('Target protocol must be HTTP or HTTPS');
  if (target.username || target.password) throw new Error('Target URL credentials are not allowed');
  if (!hostname || !allowed.has(hostname)) throw new Error('Target hostname is not allowed');
  if (hostname === 'localhost' || hostname.endsWith('.localhost') || isForbiddenIpLiteral(hostname)) {
    throw new Error('Local and private target addresses are not allowed');
  }
  target.hostname = hostname;
  return target;
}

function filteredHeaders(headers) {
  const result = new Headers();
  const connectionHeaders = new Set((headers.get('connection') || '').split(',').map(value => value.trim().toLowerCase()));
  for (const [key, value] of headers.entries()) {
    const name = key.toLowerCase();
    if (name === 'host' || name === 'content-length' || name === 'accept-encoding' ||
        SENSITIVE_HEADERS.has(name) || HOP_BY_HOP_HEADERS.has(name) || connectionHeaders.has(name)) continue;
    result.append(name, value);
  }
  result.set('accept-encoding', 'identity');
  return result;
}

async function fetchValidated(target, request, allowlistValue) {
  let current = validateTarget(target, allowlistValue);
  for (let redirects = 0; ; redirects++) {
    const response = await fetch(new Request(current, {
      method: request.method,
      headers: filteredHeaders(request.headers),
      body: ['GET', 'HEAD'].includes(request.method) ? undefined : request.body,
      redirect: 'manual',
    }));
    if (![301, 302, 303, 307, 308].includes(response.status)) return response;
    if (redirects >= MAX_REDIRECTS) throw new Error('Too many redirects');
    const location = response.headers.get('location');
    if (!location) return response;
    if (!['GET', 'HEAD'].includes(request.method)) throw new Error('Redirects for requests with bodies are not supported');
    await response.body?.cancel();
    current = validateTarget(new URL(location, current).href, allowlistValue);
  }
}

function cacheable(response) {
  const cacheControl = response.headers.get('cache-control') || '';
  const vary = (response.headers.get('vary') || '').toLowerCase();
  return response.ok && !response.headers.has('set-cookie') &&
    !/(?:^|,)\s*(?:no-store|private)(?:\s|=|,|$)/i.test(cacheControl) &&
    (!vary || vary.split(',').every(name => ['accept', 'accept-language', 'accept-encoding'].includes(name.trim())));
}

export default {
  async fetch(request, env = {}, ctx = {}) {
    if (!env.PROXY_ACCESS_TOKEN) return new Response('Proxy access token is not configured', { status: 503 });
    if (request.headers.get('x-proxy-token') !== env.PROXY_ACCESS_TOKEN) {
      return new Response('Unauthorized', { status: 401 });
    }
    const requestUrl = new URL(request.url);
    const rawTarget = requestUrl.searchParams.get('url');
    if (!rawTarget) return new Response('Missing ?url= parameter', { status: 400 });

    let target;
    try {
      target = validateTarget(rawTarget, env.PROXY_ALLOWED_HOSTS || '');
    } catch (error) {
      return new Response(error.message, { status: 400 });
    }

    const method = request.method.toUpperCase();
    const anonymous = !UPSTREAM_CREDENTIAL_HEADERS.some(name => request.headers.has(name));
    const cacheUrl = new URL(requestUrl.origin + requestUrl.pathname);
    cacheUrl.searchParams.set('target', target.href);
    cacheUrl.searchParams.set('accept', request.headers.get('accept') || '');
    cacheUrl.searchParams.set('accept-language', request.headers.get('accept-language') || '');
    const cacheKey = new Request(cacheUrl, { method: 'GET' });

    if (method === 'GET' && anonymous) {
      const cached = await caches.default.match(cacheKey);
      if (cached) return cached;
    }

    let response;
    try {
      response = await fetchValidated(target, request, env.PROXY_ALLOWED_HOSTS || '');
    } catch (error) {
      return new Response(`Proxy fetch failed: ${error.message}`, { status: 502 });
    }

    if (method === 'GET' && anonymous && cacheable(response)) {
      const cached = new Response(response.clone().body, response);
      cached.headers.set('Cache-Control', `public, max-age=${CACHE_TTL_SECONDS}`);
      const put = caches.default.put(cacheKey, cached);
      if (ctx.waitUntil) ctx.waitUntil(put);
      else await put;
    }
    return response;
  },
};
