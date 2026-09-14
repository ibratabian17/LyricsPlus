import '../shared/utils/patch-caches.js';
import { Hono } from 'hono';
import { bodyLimit } from 'hono/body-limit';
import { cors } from 'hono/cors';
import { compress } from 'hono/compress';
import { handleLyricsRequest, handleRawLyricsRequest } from '../modules/lyrics/lyrics.handler.js';
import { handleSonglistSearch } from '../modules/songCatalog/songCatalog.handler.js';
import { handleMetadataGet } from '../modules/metadata/metadata.handler.js';
import { handleChallenge, handleSubmit } from '../modules/submit/submit.handler.js';
import { runWithRequestContext, flushLogs, logger } from '../shared/utils/logger.util.js';
import { rateLimiter } from '../shared/middleware/rateLimit.middleware.js';
import { cache } from 'hono/cache';
import { HTTPException } from 'hono/http-exception';

import { SERVER } from '../shared/config.js';

const app = new Hono();

app.onError((err, c) => {
    logger.error('Unhandled error:', err);
    if (err instanceof HTTPException) {
        return err.getResponse();
    }
    return c.json({
        error: 'Internal Server Error',
        message: err.message,
        stack: err.stack,
    }, 500);
});

app.use('*', async (c, next) => {
    const requestId = crypto.randomUUID().slice(0, 8);
    c.set('requestId', requestId);
    const buffer = SERVER.DISABLE_LOGGING ? null : [];
    return runWithRequestContext({ requestId, buffer }, async () => {
        try {
            await next();
        } finally {
            flushLogs();
        }
    });
});

app.use('*', cors());
app.use('*', compress());

let concurrentRequests = 0;
const MAX_CONCURRENCY = Number(process.env.MAX_CONCURRENCY) || 15000;
app.use('*', async (c, next) => {
    if (concurrentRequests >= MAX_CONCURRENCY) {
        return c.json({ error: 'Server Busy', message: 'Too many concurrent requests. Please retry shortly. Currently we have ' + concurrentRequests + ' concurrent requests' }, 503);
    }
    concurrentRequests++;
    try {
        await next();
    } finally {
        concurrentRequests--;
    }
});

app.use('*', rateLimiter());
app.use('*', async (c, next) => {
    if (c.req.url.length > 4096) {
        return c.json({ error: 'Request URL is too long' }, 414);
    }
    const queryValues = [...new URL(c.req.url).searchParams.values()];
    if (queryValues.length > 20 || queryValues.some(value => value.length > 500)) {
        return c.json({ error: 'Query parameters exceed allowed limits' }, 400);
    }
    await next();
});

// public + s-maxage make the response cacheable by Cloudflare's edge for a full
// day while clients refresh hourly. max-age=3600 keeps browser cache short.
const cacheOptions = { cacheName: 'lyricsplus', cacheControl: 'public, max-age=3600, s-maxage=86400', wait: true };
app.use('/v1/lyrics/*', cache(cacheOptions));
app.use('/v2/lyrics/*', cache(cacheOptions));
app.use('/v1/ttml/*', cache(cacheOptions));
app.use('/v1/raw/*', cache(cacheOptions));
app.use('/v1/metadata/*', cache(cacheOptions));

app.get('/', (c) => c.text('Seems, you trying to find out about our api huh?'));

app.get('/v1/lyrics/get', (c) => {
    c.set('format', 'v1');
    return handleLyricsRequest(c);
});
app.get('/v2/lyrics/get', handleLyricsRequest);
app.get('/v1/ttml/get', (c) => {
    c.set('format', 'ttml');
    return handleLyricsRequest(c);
});
app.get('/v1/raw/get', handleRawLyricsRequest);

app.get('/v1/songlist/search', handleSonglistSearch);
app.get('/v1/metadata/get', handleMetadataGet);

app.get('/v1/lyricsplus/challenge', handleChallenge);
app.post('/v1/lyricsplus/submit', bodyLimit({
    maxSize: 1024 * 1024,
    onError: (c) => c.json({ error: 'Request body exceeds 1 MiB' }, 413),
}), handleSubmit);

export default app;
