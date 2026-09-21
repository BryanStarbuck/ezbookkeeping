// GENERATED from src/lib/errfile — run scripts/sync-errfile.sh
// Burst folding — pm/error_err.mdx R10, §11.1.
//
// The first occurrence of a key is written through. Later ones inside the window are counted, and
// ONE summary is written when the window closes (a one-shot unref'd timer per owing key), when the
// key is evicted, or at exit (`flushAll`). The tail of a burst is never lost, and the table is
// capped at FOLD_MAX_KEYS, least-recently-seen first.

/** R10: identical faults fold inside this window. */
export const FOLD_WINDOW_MS = 60_000;
/** §11.2: transient network faults fold per 10 minutes, as WARN. */
export const TRANSIENT_WINDOW_MS = 10 * 60_000;
/** §10: the fold table is capped. */
export const FOLD_MAX_KEYS = 1000;
/** The normalised message is cut here before it becomes part of the key. */
export const FOLD_KEY_MESSAGE_CAP = 300;

export type FoldSummary = {
    key: string;
    /** How many occurrences were folded (not counting the one written through). */
    count: number;
    windowMs: number;
    /** When the written-through occurrence happened (ms since the epoch). */
    firstAt: number;
};

export type BurstFolder = {
    /** True: write this occurrence. False: it was folded into the burst. */
    admit(key: string, windowMs: number, context: unknown, now?: number): boolean;
    /** Write every owed summary now (exit, fatal). */
    flushAll(): void;
    /** Test seam: keys currently tracked. */
    size(): number;
    /** Test seam: drop all state without writing. */
    reset(): void;
};

type Entry = {
    firstAt: number;
    windowMs: number;
    count: number;
    context: unknown;
    timer: ReturnType<typeof setTimeout> | null;
};

/** Unref a timer where the runtime supports it (node); a browser timer is a number. */
export function unrefTimer(timer: unknown): void {
    if (typeof timer === 'object' && timer !== null && 'unref' in timer) {
        const unref = timer.unref;

        if (typeof unref === 'function') {
            unref.call(timer);
        }
    }
}

// The normalisation steps, in the order pkg/errfile/testdata/vectors.json `normalise.steps` lists
// them. Same regex syntax in Go (RE2) and JS.
const UUID = /[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/g;
const ABS_PATH = /(?:\/[A-Za-z0-9_.\-]+){2,}/g;
const HEX_RUN = /\b[0-9a-fA-F]{8,}\b/g;
const DIGITS = /[0-9]+/g;

/** Normalise a message so `id 123` and `id 456` fold together. */
export function normalizeMessage(message: string): string {
    return message
        .replace(UUID, '<uuid>')
        .replace(ABS_PATH, '<path>')
        .replace(HEX_RUN, '<hex>')
        .replace(DIGITS, '#')
        .slice(0, FOLD_KEY_MESSAGE_CAP);
}

export function createBurstFolder(opts: {
    maxKeys: number;
    /** Write one summary line. Must not throw into the folder; it is wrapped anyway. */
    emit: (summary: FoldSummary, context: unknown) => void;
}): BurstFolder {
    const { maxKeys, emit } = opts;
    const table = new Map<string, Entry>();

    function settle(key: string, entry: Entry): void {
        if (entry.timer !== null) {
            clearTimeout(entry.timer);
            entry.timer = null;
        }

        if (entry.count > 0) {
            const summary: FoldSummary = {
                key,
                count: entry.count,
                windowMs: entry.windowMs,
                firstAt: entry.firstAt
            };
            entry.count = 0;

            try {
                emit(summary, entry.context);
            } catch {
                // the summary is worth strictly less than the line it counts (R11)
            }
        }
    }

    function closeWindow(key: string): void {
        const entry = table.get(key);

        if (!entry) {
            return;
        }

        entry.timer = null;
        table.delete(key);
        settle(key, entry);
    }

    return {
        admit(key, windowMs, context, now = Date.now()) {
            const entry = table.get(key);

            if (entry && now - entry.firstAt < entry.windowMs) {
                entry.count += 1;
                entry.context = context;
                // Least-recently-SEEN order: re-insert at the end.
                table.delete(key);
                table.set(key, entry);

                if (entry.timer === null) {
                    const remaining = Math.max(1, entry.windowMs - (now - entry.firstAt));
                    entry.timer = setTimeout(() => closeWindow(key), remaining);
                    unrefTimer(entry.timer);
                }

                return false;
            }

            if (entry) {
                table.delete(key);
                settle(key, entry);
            }

            table.set(key, {
                firstAt: now,
                windowMs,
                count: 0,
                context,
                timer: null
            });

            while (table.size > maxKeys) {
                const oldest = table.keys().next();

                if (oldest.done) {
                    break;
                }

                const evicted = table.get(oldest.value);
                table.delete(oldest.value);

                if (evicted) {
                    settle(oldest.value, evicted);
                }
            }

            return true;
        },
        flushAll() {
            for (const [key, entry] of [...table]) {
                settle(key, entry);
            }
        },
        size() {
            return table.size;
        },
        reset() {
            for (const entry of table.values()) {
                if (entry.timer !== null) {
                    clearTimeout(entry.timer);
                }
            }

            table.clear();
        }
    };
}
