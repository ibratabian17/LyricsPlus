import { fetchWithProxy } from './fetch.util.js';
import { combineSignals } from './fetch.util.js';

export async function fetchWithTimeout(resource, options = {}) {
    const { timeout = 8000 } = options;

    const controller = new AbortController();
    const id = setTimeout(() => controller.abort(), timeout);
    const combined = combineSignals([options.signal, controller.signal]);
    try {
        return await fetchWithProxy(resource, { ...options, signal: combined.signal });
    } finally {
        clearTimeout(id);
        combined.cleanup();
    }
}
