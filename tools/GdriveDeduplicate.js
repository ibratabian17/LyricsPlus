import GoogleDrive from "../src/shared/utils/googleDrive.util.js";
import { GDRIVE } from "../src/shared/config.js";

const googleDrive = new GoogleDrive();
const CONCURRENCY_LIMIT = Number(process.env.CONCURRENCY) || 30;
const DRY_RUN = process.env.DRY_RUN === "true";
const CROSS_POOL = process.env.CROSS_POOL === "true" || process.argv.includes("--cross-pool");
const TARGET_FILTER = process.argv.slice(2).find(arg => !arg.startsWith("--")) || process.env.FOLDER || "";

const RETRY_OPTIONS = {
  maxAttempts: 5,
  initialDelayMs: 1000,
  backoffFactor: 2,
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
 * Indexes files from a folder using streaming pages, keeping only lightweight structures in RAM.
 */
async function indexFolder(folderId, folderName) {
  const nameMap = new Map(); // name -> Array<{ id: string, createdTimeMs: number }>
  let totalFiles = 0;
  let duplicateCount = 0;

  for await (const { files } of googleDrive.listFilesStream(folderId, {
    fields: "files(id,name,createdTime)",
    pageSize: 1000
  })) {
    if (isAborted) break;

    for (const f of files) {
      totalFiles++;
      const createdTimeMs = f.createdTime ? new Date(f.createdTime).getTime() : 0;
      const item = { id: f.id, createdTimeMs, name: f.name };

      const existing = nameMap.get(f.name);
      if (!existing) {
        nameMap.set(f.name, [item]);
      } else {
        existing.push(item);
        if (existing.length === 2) {
          duplicateCount++;
        }
      }
    }

    process.stdout.write(
      `\r[${folderName}] Indexing files... (${totalFiles} indexed, ${duplicateCount} duplicate${duplicateCount === 1 ? '' : 's'} found)`
    );
  }

  process.stdout.write(
    `\r[${folderName}] Indexed ${totalFiles} files (${duplicateCount} duplicate${duplicateCount === 1 ? '' : 's'} found).\n`
  );

  return { nameMap, totalFiles };
}

/**
 * Deletes duplicate files concurrently using continuous worker pool.
 */
async function deleteDuplicates(toDelete, folderName) {
  if (toDelete.length === 0) return 0;

  let deleted = 0;
  let index = 0;
  const failures = [];
  const startTime = Date.now();

  const updateProgress = () => {
    const elapsedSec = (Date.now() - startTime) / 1000;
    const speed = elapsedSec > 0 ? (deleted / elapsedSec).toFixed(1) : "0.0";
    const remaining = toDelete.length - deleted;
    const etaSec = Number(speed) > 0 ? Math.round(remaining / Number(speed)) : 0;
    const etaMin = (etaSec / 60).toFixed(1);
    process.stdout.write(
      `\r[${folderName}] ${deleted}/${toDelete.length} ${DRY_RUN ? "would be deleted" : "deleted"} (${speed}/s, ETA: ${etaMin}m)`
    );
  };

  async function worker() {
    while (index < toDelete.length && !isAborted) {
      const file = toDelete[index++];
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
          `${folderName} / "${file.name}" (${file.id})`
        );
        deleted++;
        updateProgress();
      } catch (err) {
        failures.push({ file, err });
      }
    }
  }

  const numWorkers = Math.min(CONCURRENCY_LIMIT, toDelete.length);
  await Promise.all(Array.from({ length: numWorkers }, () => worker()));
  process.stdout.write("\n");

  if (failures.length > 0) {
    console.warn(`[${folderName}] Warning: ${failures.length} deletion(s) failed.`);
  }

  return deleted;
}

/**
 * Deduplicates files inside a single folder.
 */
async function deduplicateFolder(folderId, folderName) {
  console.log(`[${folderName}] Scanning for duplicates...`);

  const { nameMap, totalFiles } = await indexFolder(folderId, folderName);

  if (totalFiles === 0) {
    console.log(`[${folderName}] Folder is empty, skipping.`);
    return { count: 0, survivingMap: new Map() };
  }

  const toDelete = [];
  const survivingMap = new Map(); // name -> { id, createdTimeMs, folderName }

  for (const [name, group] of nameMap) {
    if (group.length > 1) {
      // Sort newest first
      group.sort((a, b) => b.createdTimeMs - a.createdTimeMs);
      const [newest, ...duplicates] = group;
      survivingMap.set(name, { id: newest.id, createdTimeMs: newest.createdTimeMs, folderName });
      toDelete.push(...duplicates);
    } else if (group.length === 1) {
      survivingMap.set(name, { id: group[0].id, createdTimeMs: group[0].createdTimeMs, folderName });
    }
  }

  nameMap.clear(); // Free memory immediately

  if (toDelete.length === 0) {
    console.log(`[${folderName}] No within-folder duplicates found.`);
    return { count: 0, survivingMap };
  }

  console.log(
    `[${folderName}] Found ${toDelete.length} duplicate(s). ${DRY_RUN ? "Would delete" : "Deleting"} oldest copies (concurrency: ${CONCURRENCY_LIMIT})...`
  );

  const totalDeleted = await deleteDuplicates(toDelete, folderName);
  console.log(`[${folderName}] Done. ${DRY_RUN ? "Would remove" : "Removed"} ${totalDeleted} duplicate(s).`);

  return { count: totalDeleted, survivingMap };
}

/**
 * Deduplicates files across multiple folders belonging to the same service pool.
 */
async function deduplicateAcrossPool(folderSurvivingMaps, serviceName) {
  console.log(`\n[${serviceName}] Checking for cross-folder duplicates across ${folderSurvivingMaps.length} folders...`);

  const globalNameMap = new Map(); // name -> { id, createdTimeMs, folderName }
  const crossDuplicates = [];

  for (const survivingMap of folderSurvivingMaps) {
    for (const [name, fileInfo] of survivingMap) {
      const existing = globalNameMap.get(name);
      if (!existing) {
        globalNameMap.set(name, fileInfo);
      } else {
        if (fileInfo.createdTimeMs > existing.createdTimeMs) {
          crossDuplicates.push({ id: existing.id, name, folderName: existing.folderName });
          globalNameMap.set(name, fileInfo);
        } else {
          crossDuplicates.push({ id: fileInfo.id, name, folderName: fileInfo.folderName });
        }
      }
    }
    survivingMap.clear(); // Free memory
  }

  if (crossDuplicates.length === 0) {
    console.log(`[${serviceName}] No cross-folder duplicates found across pools.`);
    return 0;
  }

  console.log(`[${serviceName}] Found ${crossDuplicates.length} cross-folder duplicate(s). Deleting older copies...`);
  return await deleteDuplicates(crossDuplicates, `${serviceName} (Cross-Pool)`);
}

async function main() {
  console.log("========================================");
  console.log("   Google Drive Cache Deduplication     ");
  console.log(`   Concurrency: ${CONCURRENCY_LIMIT} | Dry Run: ${DRY_RUN} | Cross-Pool: ${CROSS_POOL}`);
  if (TARGET_FILTER) console.log(`   Filter: "${TARGET_FILTER}"`);
  console.log("========================================\n");

  const services = [
    { folderId: GDRIVE.CACHED_SPOTIFY,    name: "Spotify"     },
    { folderId: GDRIVE.CACHED_TTML,       name: "Apple Music" },
    { folderId: GDRIVE.CACHED_MUSIXMATCH, name: "Musixmatch"  },
    { folderId: GDRIVE.CACHED_QQ,         name: "QQ Music"    },
    { folderId: GDRIVE.CACHED_DEEZER,     name: "Deezer"      },
  ];

  let grandTotal = 0;

  for (const { folderId, name } of services) {
    const ids = Array.isArray(folderId) ? folderId : [folderId];
    const folderSurvivingMaps = [];

    for (const [index, id] of ids.entries()) {
      if (!id || isAborted) continue;
      const folderName = `${name}${ids.length > 1 ? ` #${index + 1}` : ""}`;

      if (TARGET_FILTER && !folderName.toLowerCase().includes(TARGET_FILTER.toLowerCase())) {
        continue;
      }

      const { count, survivingMap } = await deduplicateFolder(id, folderName);
      grandTotal += count;
      if (CROSS_POOL && survivingMap.size > 0) {
        folderSurvivingMaps.push(survivingMap);
      }
    }

    if (CROSS_POOL && folderSurvivingMaps.length > 1 && !isAborted) {
      const crossDeleted = await deduplicateAcrossPool(folderSurvivingMaps, name);
      grandTotal += crossDeleted;
    }
  }

  console.log("\n========================================");
  if (grandTotal === 0) {
    console.log("No duplicates found. All scanned folders are clean!");
  } else {
    console.log(`Deduplication complete. Total removed: ${grandTotal} file(s).`);
  }
  console.log("========================================");
}

main().catch((error) => {
  console.error("\nDeduplication failed:", error);
  process.exitCode = 1;
});
