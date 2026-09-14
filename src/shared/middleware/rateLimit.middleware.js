const rateLimitCache = new Map();
const MAX_REQUESTS = Number(process.env.RATE_LIMIT_MAX) || 20;
const LIMIT_MS = Number(process.env.RATE_LIMIT_WINDOW_MS) || 10000;
const MAX_CACHE_ENTRIES = 5000;
const PURGE_INTERVAL_MS = Math.max(LIMIT_MS, 15000);
let _lastPurge = Date.now();

function getClientKey(c) {
    if (c.env?.MY_RATE_LIMITER) {
        return c.req.header('cf-connecting-ip') || 'cloudflare-unknown';
    }

    const cfConnectingIp = c.req.header('cf-connecting-ip');
    if (cfConnectingIp) return cfConnectingIp;

    const xVercelForwardedFor = c.req.header('x-vercel-forwarded-for');
    if (xVercelForwardedFor) return xVercelForwardedFor.split(',')[0].trim();

    const xForwardedFor = c.req.header('x-forwarded-for');
    if (xForwardedFor) return xForwardedFor.split(',')[0].trim();

    const xRealIp = c.req.header('x-real-ip');
    if (xRealIp) return xRealIp;

    if (typeof Bun !== 'undefined') {
        try {
            const server = c.env && ('server' in c.env ? c.env.server : c.env);
            if (server && typeof server.requestIP === 'function') {
                const info = server.requestIP(c.req.raw);
                if (info?.address) return info.address;
            }
        } catch (e) {
            // Ignore error and fall through
        }
    }

    if (c.env?.incoming?.socket?.remoteAddress) {
        return c.env.incoming.socket.remoteAddress;
    }

    if (c.env?.incoming?.connection?.remoteAddress) {
        return c.env.incoming.connection.remoteAddress;
    }

    return 'direct-unknown';
}

function purgeExpiredEntries(now) {
    _lastPurge = now;
    for (const [key, entry] of rateLimitCache) {
        if (now > entry.resetAt) {
            rateLimitCache.delete(key);
        }
    }
}

export const rateLimiter = () => {
    return async (c, next) => {
        if (c.req.method === 'OPTIONS') return await next();

        const ip = getClientKey(c);

        if (c.env?.MY_RATE_LIMITER) {
            const { success } = await c.env.MY_RATE_LIMITER.limit({ key: ip });
            if (!success) {
                return c.json({
                    error: 'Too Many Requests',
                    message: `Rate limit exceeded. Please wait 10 seconds before trying again (${MAX_REQUESTS} requests per 10 seconds allowed).`
                }, 429);
            }
        } else {
            const now = Date.now();
            let entry = rateLimitCache.get(ip);

            if (!entry || now > entry.resetAt) {
                entry = { count: 1, resetAt: now + LIMIT_MS };
                
                // Evict oldest if map exceeds maximum capacity
                if (rateLimitCache.size >= MAX_CACHE_ENTRIES && !rateLimitCache.has(ip)) {
                    const oldestKey = rateLimitCache.keys().next().value;
                    if (oldestKey) rateLimitCache.delete(oldestKey);
                }
                rateLimitCache.set(ip, entry);
            } else {
                entry.count++;
                if (entry.count > MAX_REQUESTS) {
                    const remainingSecs = Math.max(1, Math.ceil((entry.resetAt - now) / 1000));
                    return c.json({
                        error: 'Too Many Requests',
                        message: `Rate limit exceeded. Please wait ${remainingSecs} seconds before trying again (${MAX_REQUESTS} requests per ${Math.round(LIMIT_MS / 1000)} seconds allowed).`
                    }, 429);
                }
            }

            // Periodic lightweight cleanup
            if (rateLimitCache.size > 200 && now - _lastPurge > PURGE_INTERVAL_MS) {
                purgeExpiredEntries(now);
            }
        }

        await next();
    };
};
