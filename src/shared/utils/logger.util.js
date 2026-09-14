import { AsyncLocalStorage } from 'node:async_hooks';
import { SERVER } from '../config.js';

const requestContext = new AsyncLocalStorage();
const MAX_BUFFER_LOGS = 100;

export function runWithRequestContext(context, fn) {
    return requestContext.run(context, fn);
}

export function getRequestId() {
    return requestContext.getStore()?.requestId || null;
}

function formatArgs(args) {
    return args.map(a => {
        if (typeof a === 'string') return a;
        if (a instanceof Error) return `${a.message}\n${a.stack}`;
        try { return JSON.stringify(a); } catch { return String(a); }
    }).join(' ');
}

function emit(level, args) {
    if (SERVER.DISABLE_LOGGING) return;
    const store = requestContext.getStore();
    const id = store?.requestId;
    const prefix = id ? `[${id}] ` : '';

    // Appwrite runtime: use the request-scoped log/error functions
    if (store?.appwriteLog) {
        const msg = prefix + formatArgs(args);
        if (level === 'error') {
            store.appwriteError(msg);
        } else {
            store.appwriteLog(msg);
        }
        return;
    }

    // Local dev: buffer logs for grouped output
    const formatted = id ? [`[${id}]`, ...args] : args;
    if (store?.buffer) {
        if (store.buffer.length < MAX_BUFFER_LOGS) {
            store.buffer.push({ level, args: formatted });
        } else if (store.buffer.length === MAX_BUFFER_LOGS) {
            store.buffer.push({ level: 'warn', args: [`[${id || '?'}] ... logs truncated (max ${MAX_BUFFER_LOGS} per request reached)`] });
        }
    } else {
        console[level](...formatted);
    }
}

export function flushLogs() {
    if (SERVER.DISABLE_LOGGING) return;
    const store = requestContext.getStore();
    if (!store?.buffer || store.buffer.length === 0) return;

    const id = store.requestId || '?';
    console.log(`── req ${id} (${store.buffer.length} logs) ──`);
    for (const entry of store.buffer) {
        console[entry.level](...entry.args);
    }
    console.log(`── end ${id} ──`);
    store.buffer.length = 0;
}

export const logger = {
    log:   (...args) => emit('log', args),
    debug: (...args) => emit('debug', args),
    warn:  (...args) => emit('warn', args),
    error: (...args) => emit('error', args),
};