import GoogleDrive from "../src/shared/utils/googleDrive.util.js";
import { GDRIVE } from "../src/shared/config.js";

const googleDrive = new GoogleDrive();
const CONCURRENCY_LIMIT = Number(process.env.CONCURRENCY) || 30;
const DRY_RUN = process.env.DRY_RUN === "true";
const TARGET_FILTER = process.argv.slice(2).find(arg => !arg.startsWith("--")) || process.env.FOLDER || "";
const SIX_MONTHS_MS = 6 * 30 * 24 * 60 * 60 * 1000; // Approximate 6 months in milliseconds

const RETRY_OPTIONS = {
  maxAttempts: 5,
  initialDelayMs: 1000,  // 1 s
  backoffFactor: 2,      // 1 s → 2 s → 4 s → 8 s → 16 s
  retryableStatuses: new Set([429, 500, 502, 503, 504]),
};

let isAborted = false;
const sigintHandler = () => {
  if (isAborted) process.exit(1);
  isAborted = true;
  console.log(`\nGracefully stopping after in-flight requests finish... (Press Ctrl+C again to force exit)`);
};
process.on("SIGINT", sigintHandler);

async function retryWithBackoff(fn, label = "") {
  const { maxAttempts, initialDelayMs, backoffFactor, retryableStatuses } = RETRY_OPTIONS;
  let delay = initialDelayMs;

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      return await fn();
    } catch (err) {
      const status =
        err?.status ??
        err?.code ??
        Number(String(err?.message).match(/\b(4\d{2}|5\d{2})\b/)?.[0]);

      const isRetryable = retryableStatuses.has(status);
      const isLastAttempt = attempt === maxAttempts;

      if (!isRetryable || isLastAttempt) throw err;

      console.warn(
        `\n[retry] ${label} – attempt ${attempt}/${maxAttempts} failed (HTTP ${status || "?"}). Retrying in ${delay}ms...`
      );
      await new Promise((res) => setTimeout(res, delay + Math.random() * 500));
      delay *= backoffFactor;
    }
  }
}

/**
 * Deletes a batch of files concurrently using a worker pool.
 */
async function deleteBatch(files, cacheName, stats) {
  let index = 0;
  const numWorkers = Math.min(CONCURRENCY_LIMIT, files.length);

  async function worker() {
    while (index < files.length && !isAborted) {
      const file = files[index++];
      try {
        await retryWithBackoff(
          async () => {
            if (DRY_RUN) return true;
            try {
              return await googleDrive.deleteFile(file.id, { skipInvalidate: true });
            } catch (err) {
              const status = err?.status ?? err?.code;
              if (status === 404 || String(err?.message).includes("404")) return true;
              throw err;
            }
          },
          `${cacheName} / "${file.name}" (${file.id})`
        );
        stats.deleted++;
        stats.updateProgress();
      } catch (err) {
        stats.failures.push({ file, err });
      }
    }
  }

  await Promise.all(Array.from({ length: numWorkers }, () => worker()));
}

/**
 * Cleans up old cache files (older than 6 months) using server-side query filtering and streaming.
 */
async function cleanupOldCacheStream(folderId, cacheName) {
  const cutoffDate = new Date(Date.now() - SIX_MONTHS_MS);
  const cutoffIso = cutoffDate.toISOString();
  console.log(`[${cacheName}] Scanning for files older than ${cutoffDate.toISOString().slice(0, 10)}...`);

  const startTime = Date.now();
  const stats = {
    deleted: 0,
    failures: [],
    updateProgress() {
      const elapsedSec = (Date.now() - startTime) / 1000;
      const speed = elapsedSec > 0 ? (stats.deleted / elapsedSec).toFixed(1) : "0.0";
      process.stdout.write(
        `\r[${cacheName}] ${stats.deleted} old file(s) ${DRY_RUN ? "would be deleted" : "deleted"} (${speed}/s)`
      );
    }
  };

  // Google Drive server-side query: modifiedTime < cutoff
  const extraQuery = `modifiedTime < '${cutoffIso}'`;
  let pageIndex = 0;
  let totalFound = 0;

  for await (const { files } of googleDrive.listFilesStream(folderId, {
    extraQuery,
    fields: "files(id,name,modifiedTime,createdTime)",
    pageSize: 1000
  })) {
    if (isAborted) break;

    pageIndex++;
    totalFound += files.length;
    if (files.length === 0 && pageIndex === 1) {
      console.log(`[${cacheName}] No files older than 6 months found.`);
      return 0;
    }

    await deleteBatch(files, cacheName, stats);
  }

  if (totalFound > 0) {
    process.stdout.write("\n");
    if (stats.failures.length > 0) {
      console.warn(`[${cacheName}] ${stats.failures.length} deletion(s) failed.`);
    }
    console.log(`[${cacheName}] Done. Total removed: ${stats.deleted} old file(s).`);
  } else if (pageIndex > 1) {
    console.log(`\n[${cacheName}] Done. No more old files found.`);
  }

  return stats.deleted;
}

async function main() {
  console.log("========================================");
  console.log("   Google Drive Cleanup (Older Files)   ");
  console.log(`   Threshold: > 6 Months | Concurrency: ${CONCURRENCY_LIMIT} | Dry Run: ${DRY_RUN}`);
  if (TARGET_FILTER) console.log(`   Filter: "${TARGET_FILTER}"`);
  console.log("========================================\n");

  const caches = [
    { folderId: GDRIVE.CACHED_SPOTIFY,    name: "Spotify"     },
    { folderId: GDRIVE.CACHED_TTML,       name: "Apple Music" },
    { folderId: GDRIVE.CACHED_MUSIXMATCH, name: "Musixmatch"  },
    { folderId: GDRIVE.CACHED_QQ,         name: "QQ Music"    },
    { folderId: GDRIVE.CACHED_DEEZER,     name: "Deezer"      },
  ];

  let grandTotal = 0;

  for (const { folderId, name } of caches) {
    const ids = Array.isArray(folderId) ? folderId : [folderId];
    for (const [index, id] of ids.entries()) {
      if (!id || isAborted) continue;
      const folderName = `${name}${ids.length > 1 ? ` #${index + 1}` : ""}`;

      if (TARGET_FILTER && !folderName.toLowerCase().includes(TARGET_FILTER.toLowerCase())) {
        continue;
      }

      grandTotal += await cleanupOldCacheStream(id, folderName);
    }
  }

  console.log("\n========================================");
  if (grandTotal === 0) {
    console.log("No old files found. All caches are fresh!");
  } else {
    console.log(`Cleanup complete. Total removed: ${grandTotal} old file(s).`);
  }
  console.log("========================================");
}

main().catch((error) => {
  console.error("Cleanup failed:", error);
  process.exitCode = 1;
});
