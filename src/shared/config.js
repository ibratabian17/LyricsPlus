export const LYRICSPLUS = {
    JWT_SECRET: process.env.JWT_SECRET || "lyricsplus-submit-opensource-yes-yes-yes",
    ALLOW_SUBMISSIONS: false, //default USERTML_JSON are locked to official apis, set to true to allow crowdsourced submission
};

export const SERVER = {
    PORT: process.env.PORT || 3000,
    DISABLE_LOGGING: process.env.DISABLE_LOGGING === 'true',
    PROXY: {
        ENABLED: false,
        ACCESS_TOKEN: process.env.PROXY_ACCESS_TOKEN || "",
        URLS: [
            "https://proxy-lyplus.prjktla.workers.dev/?url="
        ],
    },
}

// Environment runtime detection

export const CACHE_CONFIG = {
    GDRIVE_ENABLED: true,
    // On by default. Queries run in a worker thread (db.worker.js), so the DB
    // no longer blocks the main event loop; set DB_ENABLED=false to disable.
    DB_ENABLED: process.env.DB_ENABLED !== 'false',
    DB_PATH: (typeof process !== 'undefined' ? process.env.CACHE_DB_PATH : undefined) || 'database/lyrics_cache.db',
};


/**
 * Parses a comma-separated env variable into an array of non-empty folder IDs.
 * Falls back to the provided default string (also comma-splittable).
 * @param {string|undefined} envValue
 * @param {string} defaultValue
 * @returns {string[]}
 */
function parseFolderIds(envValue, defaultValue) {
    const raw = envValue || defaultValue || '';
    return raw.split(',').map(s => s.trim()).filter(Boolean);
}

export const GDRIVE = {
    // Each value is an array of folder IDs. The first is the primary; extras are fallbacks.
    // Set via env as comma-separated IDs, e.g. GDRIVE_CACHED_SPOTIFY="id1,id2,id3"
    CACHED_SPOTIFY: parseFolderIds(process.env.GDRIVE_CACHED_SPOTIFY, ""), //Spotify
    CACHED_TTML: parseFolderIds(process.env.GDRIVE_CACHED_TTML, ""), //Apple Music
    USERTML_JSON: parseFolderIds(process.env.GDRIVE_USERTML_JSON, "1RFoNsI5wAsRjQSVDOMaotDmMZNIQOWnW"), //Lyrics+
    CACHED_MUSIXMATCH: parseFolderIds(process.env.GDRIVE_CACHED_MUSIXMATCH, ""), //Musixmatch
    CACHED_QQ: parseFolderIds(process.env.GDRIVE_CACHED_QQ, ""), //QQ Music
    CACHED_DEEZER: parseFolderIds(process.env.GDRIVE_CACHED_DEEZER, ""),
    API_URL: "https://www.googleapis.com/drive/v3/files/",
    API_URL_UPDATE: "https://www.googleapis.com/upload/drive/v2/files/",
};

function parseGDriveAccounts(envValue, defaultAccounts) {
    if (!envValue) return defaultAccounts;
    try {
        const parsed = JSON.parse(envValue);
        if (Array.isArray(parsed) && parsed.length > 0) return parsed;
    } catch {
        // Fallback: expect format "CLIENT_ID1|CLIENT_SECRET1|REFRESH_TOKEN1,CLIENT_ID2|CLIENT_SECRET2|REFRESH_TOKEN2"
        const accounts = envValue.split(',').map(item => {
            const [client_id, client_secret, refresh_token, root] = item.split('|').map(s => s.trim());
            if (client_id && client_secret && refresh_token) {
                return { CLIENT_ID: client_id, CLIENT_SECRET: client_secret, REFRESH_TOKEN: refresh_token, ROOT: root || "" };
            }
            return null;
        }).filter(Boolean);
        if (accounts.length > 0) return accounts;
    }
    return defaultAccounts;
}

const DEFAULT_GDRIVE_ACCOUNT = {
    CLIENT_ID: process.env.AUTH_KEY_CLIENT_ID || "",
    CLIENT_SECRET: process.env.AUTH_KEY_CLIENT_SECRET || "",
    REFRESH_TOKEN: process.env.AUTH_KEY_REFRESH_TOKEN || "",
    ROOT: process.env.AUTH_KEY_ROOT || "",
};

export const AUTH_KEY = {
    ...DEFAULT_GDRIVE_ACCOUNT,
    ACCOUNTS: parseGDriveAccounts(process.env.GDRIVE_ACCOUNTS, [DEFAULT_GDRIVE_ACCOUNT]),
};

export const APPLE_MUSIC = {
    BASE_URL: "https://amp-api.music.apple.com/v1",
    EDGE_BASE_URL: "https://amp-api-edge.music.apple.com/v1",
    ACCOUNTS: [
        {
            NAMEID: "ExampleAndroid",
            AUTH_TYPE: "android", 
            ANDROID_AUTH_TOKEN: process.env.APPLE_MUSIC_ANDROID_AUTH_TOKEN || "",
            ANDROID_DSID: process.env.APPLE_MUSIC_ANDROID_DSID || "",
            ANDROID_USER_AGENT: process.env.APPLE_MUSIC_ANDROID_USER_AGENT || "Music/6.1 Android/15 model/XiaomiPOCOF1 build/1451 (dt:66)",
            ANDROID_COOKIE: process.env.APPLE_MUSIC_ANDROID_COOKIE || "",
            STOREFRONT: "in", //country example: id or en or us or in
        },
        {
            NAMEID: "ExampleWeb",
            AUTH_TYPE: "web",
            MUSIC_AUTH_TOKEN: process.env.APPLE_MUSIC_AUTH_TOKEN || "",
        }
    ]
};

export const SPOTIFY = {
    BASE_URL: "https://api.spotify.com/v1",
    LYRICS_URL: "https://spclient.wg.spotify.com/color-lyrics/v2/track/",
    AUTH_URL: "https://accounts.spotify.com/api/token",
    TOKEN_URL: "https://open.spotify.com/get_access_token?reason=transport&productType=web_player",
    ACCOUNTS: [
        {
            CLIENT_ID: process.env.SPOTIFY_CLIENT_ID || "",
            CLIENT_SECRET: process.env.SPOTIFY_CLIENT_SECRET || "",
            COOKIE: process.env.SPOTIFY_COOKIE || ""
        }
    ]
};

export const DEEZER = {
    AUTH_URL: process.env.DEEZER_AUTH_URL || "https://auth.deezer.com/login/renew?jo=p&rto=c&i=c",
    GRAPHQL_URL: process.env.DEEZER_GRAPHQL_URL || "https://pipe.deezer.com/api",
    SEARCH_URL: process.env.DEEZER_SEARCH_URL || "https://api.deezer.com/search/track",
    ACCOUNTS: [
        {
            NAMEID: "DeezerDefault",
            AUTH_TYPE: "refresh-token",
            REFRESH_TOKEN: process.env.DEEZER_REFRESH_TOKEN || "",
            ARL: process.env.DEEZER_ARL || "",
        }
    ]
};

export const MUSIXMATCH = {
    ACCOUNTS: [
        {
            NAMEID: "Musixmatch-Guest",
            AUTH_TYPE: "web",
            USER_AGENT: process.env.MUSIXMATCH_USER_AGENT || 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36',
            COOKIE: process.env.MUSIXMATCH_COOKIE || 'AWSELB=55578B011601B1EF8BC274C33F9043CA947F99DCFF0A80541772015CA2B39C35C0F9E1C932D31725A7310BCAEB0C37431E024E2B45320B7F2C84490C2C97351FDE34690157'
        }
    ]
};

export class AccountManager {
    constructor(accounts) {
        this.accounts = accounts;
        this.currentIndex = 0;
    }

    getCurrentAccount() {
        if (this.accounts.length === 0) {
            return null;
        }
        return this.accounts[this.currentIndex];
    }

    getNextAccount(account) {
        if (this.accounts.length <= 1) return null;
        const index = this.accounts.indexOf(account);
        return this.accounts[(Math.max(index, 0) + 1) % this.accounts.length];
    }

    switchToNextAccount() {
        if (this.accounts.length <= 1) {
            console.warn("Only one account available, cannot switch.");
            return false;
        }
        this.currentIndex = (this.currentIndex + 1) % this.accounts.length;
        console.log(`Switched to account index: ${this.currentIndex}`);
        return true;
    }

    resetAccount() {
        this.currentIndex = 0;
        console.log("Account index reset to 0.");
    }
}

export const appleMusicAccountManager = new AccountManager(APPLE_MUSIC.ACCOUNTS);
export const spotifyAccountManager = new AccountManager(SPOTIFY.ACCOUNTS);
export const musixmatchAccountManager = new AccountManager(MUSIXMATCH.ACCOUNTS);
export const deezerAccountManager = new AccountManager(DEEZER.ACCOUNTS);
