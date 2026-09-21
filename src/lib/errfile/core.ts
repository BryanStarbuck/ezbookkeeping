// The report path — pm/error_err.mdx §5.2, §6.2, §11.
//
// Nothing in this file runs until an error happens (R3). `errorFileFor` builds one small object per
// module; every method runs only inside a catch. The report path is:
//
//   answer shape? → already reported? → mark (WeakSet) → fold? → describe + redact → sink
//                                                                  (or the pre-install queue)
//
// The file imports nothing outside src/lib/errfile: it is copied verbatim into mcp/src/errfile.

import { causesText, describeChain, describeOne, describeThrowable, headlineText, prop, safeString, stackOf } from './describe.js';
import type { DescribedError } from './describe.js';
import { FOLD_MAX_KEYS, FOLD_WINDOW_MS, TRANSIENT_WINDOW_MS, createBurstFolder, normalizeMessage } from './fold.js';
import type { BurstFolder, FoldSummary } from './fold.js';
import { clockTime, formatRecord, summaryText } from './format.js';
import type { ErrorLevel, ErrorRecord } from './format.js';
import { redactData, redactText } from './redact.js';
import type { ErrorData } from './redact.js';

export type { ErrorData, ErrorDataValue } from './redact.js';
export type { ErrorLevel, ErrorRecord, RedactedData } from './format.js';

/** Where records go once a host has booted (§5.3 browser, §5.4 node). */
export type ErrorSink = {
    /** The runtime tag stamped on records that do not carry one (§3.3). */
    app: string;
    /** Echo each written record through console.error / console.warn (§11.5). Off in the browser. */
    echo: boolean;
    /** Write EXPECTED records too (EZBK_ERROR_FILE_VERBOSE=1) — R6. */
    verbose: boolean;
    write(record: ErrorRecord): void;
    /** Synchronous barrier (node) / best-effort send (browser). */
    flush(): void;
};

export type ErrorFile = {
    /** The repo-relative source path this object reports as (R14). */
    readonly where: string;
    /** ERROR → error.err. An answer shape (§11.4) writes nothing. */
    caught(doing: string, err: unknown, data?: ErrorData): void;
    /** WARN → error.err. The error is optional: a standing condition can be a WARN on its own. */
    warn(doing: string, err?: unknown, data?: ErrorData): void;
    /** Nothing (EXPECTED under EZBK_ERROR_FILE_VERBOSE=1). The call records a DECISION — R6. */
    expected(doing: string, err: unknown): void;
    /** ERROR, then throw the SAME object (R5). */
    rethrow(doing: string, err: unknown, data?: ErrorData): never;
    /** FATAL, then a synchronous flush (node) — R9. */
    fatal(doing: string, err: unknown, data?: ErrorData): void;
};

/** Records held before any sink is installed (a module can fault at import time). */
export const PRE_INSTALL_CAP = 200;

/** The `where` the library itself reports as. */
const LIBRARY_WHERE = 'src/lib/errfile/core.ts';

type FoldContext = {
    level: ErrorLevel;
    app: string;
    where: string;
    doing: string;
    headline: string;
};

type State = {
    sink: ErrorSink | null;
    queue: ErrorRecord[];
    overflow: number;
    reported: WeakSet<object>;
    folder: BurstFolder;
    busy: boolean;
    consoleFallbackUsed: boolean;
};

function isFoldContext(value: unknown): value is FoldContext {
    return typeof value === 'object' && value !== null && 'headline' in value;
}

function createState(): State {
    const s: State = {
        sink: null,
        queue: [],
        overflow: 0,
        reported: new WeakSet<object>(),
        folder: createBurstFolder({
            maxKeys: FOLD_MAX_KEYS,
            emit: (summary: FoldSummary, context: unknown) => {
                if (!isFoldContext(context)) {
                    return;
                }

                deliver(s, {
                    ts: new Date().toISOString(),
                    level: context.level,
                    app: context.app,
                    where: context.where,
                    doing: context.doing,
                    error: summaryText(summary.count, summary.windowMs, clockTime(summary.firstAt), context.headline),
                    cause: '',
                    stack: '',
                    data: null
                });
            }
        }),
        busy: false,
        consoleFallbackUsed: false
    };
    return s;
}

const state: State = createState();

/** Read an environment variable in any runtime (a browser has none). */
export function readEnv(name: string): string | undefined {
    try {
        const g: { process?: { env?: Record<string, string | undefined> } } = globalThis;
        return g.process?.env?.[name];
    } catch {
        return undefined;
    }
}

// ── The path (§3.1) ────────────────────────────────────────────────────────────────────────────

export type ErrorFilePathOptions = {
    /** Under a test runner (R13). Default: `VITEST` or `NODE_ENV=test` in `env`. */
    test?: boolean;
    /** The numeric user id, for the fallback directories. */
    uid?: string | number;
    /** The home directory when `env.HOME` is unset (node passes `os.homedir()`). */
    home?: string;
    /** The temp directory when `env.TMPDIR` is unset (node passes `os.tmpdir()`). Default `/tmp`. */
    tmpdir?: string;
};

function trimTrailingSlash(value: string): string {
    return value.length > 1 ? value.replace(/[\\/]+$/, '') : value;
}

/**
 * §3.1, in order: `EZBK_ERROR_FILE`; the test path (R13); `~/T/ezbookkeeping/error.err`; and,
 * with no home directory, `$TMPDIR/ezbookkeeping_<uid>/error.err`. Pure: every process resolves
 * the path with this one function, and the vector file has a case for each row.
 */
export function resolveErrorFilePath(env: Record<string, string | undefined>, opts: ErrorFilePathOptions = {}): string {
    const override = env['EZBK_ERROR_FILE'];

    if (override) {
        return override;
    }

    const test = opts.test ?? (!!env['VITEST'] || env['NODE_ENV'] === 'test');
    const uid = opts.uid === undefined || opts.uid === '' ? 'user' : String(opts.uid);
    const tmp = trimTrailingSlash(env['TMPDIR'] || opts.tmpdir || '/tmp');

    if (test) {
        return `${tmp}/ezbookkeeping_test_${uid}/error.err`;
    }

    const home = trimTrailingSlash(env['HOME'] || opts.home || '');

    if (home) {
        return `${home}/T/ezbookkeeping/error.err`;
    }

    return `${tmp}/ezbookkeeping_${uid}/error.err`;
}

// ── Answer shapes (§11.4, R7) ──────────────────────────────────────────────────────────────────

/** The own keys a plain rejection object may have and still be an answer, not a fault. */
export const ANSWER_KEYS: readonly string[] = ['message', 'error', 'processed', 'canceled', 'isUpToDate', 'errorCode', 'errorMessage', 'response'];

const ANSWER_KEY_SET = new Set(ANSWER_KEYS);

/**
 * Upstream's stores reject with plain objects when the failure is an answer rather than a fault.
 * Such a value is not an `Error`, has no `stack`, and its own keys are a non-empty subset of
 * ANSWER_KEYS (an `error` key holds an object with an `errorCode`).
 */
export function isAnswer(value: unknown): boolean {
    if (typeof value !== 'object' || value === null || value instanceof Error || Array.isArray(value)) {
        return false;
    }

    let keys: string[];

    try {
        keys = Object.keys(value);
    } catch {
        return false;
    }

    if (!keys.length || prop(value, 'stack') !== undefined) {
        return false;
    }

    for (const key of keys) {
        if (!ANSWER_KEY_SET.has(key)) {
            return false;
        }
    }

    if (keys.includes('error')) {
        const inner = prop(value, 'error');

        if (typeof inner !== 'object' || inner === null || prop(inner, 'errorCode') === undefined) {
            return false;
        }
    }

    return true;
}

// ── Transient network faults (§11.2) and chunk-load failures (§11.3) ─────────────────────────

/** Codes and message fragments that mean "offline", not "fault". */
export const TRANSIENT_NETWORK_MARKERS: readonly string[] = [
    'ECONNREFUSED', 'ENOTFOUND', 'EAI_AGAIN', 'ETIMEDOUT', 'ECONNRESET', 'ERR_NETWORK', 'ECONNABORTED', 'Network Error', 'network unreachable'
];

/** Dynamic-import chunk-load failures are a WARN (§11.3). */
export const CHUNK_LOAD_MARKERS: readonly string[] = [
    'Failed to fetch dynamically imported module', 'Importing a module script failed', 'error loading dynamically imported module'
];

function hasMarker(text: unknown, markers: readonly string[]): boolean {
    if (typeof text !== 'string' || !text) {
        return false;
    }

    for (const marker of markers) {
        if (text.includes(marker)) {
            return true;
        }
    }

    return false;
}

/** §11.2 — offline is not a fault. Looks at the error and two links of its cause chain. */
export function isTransientNetworkError(err: unknown): boolean {
    let current: unknown = err;

    for (let depth = 0; depth < 3 && current !== null && current !== undefined; depth++) {
        if (hasMarker(prop(current, 'code'), TRANSIENT_NETWORK_MARKERS) || hasMarker(prop(current, 'message'), TRANSIENT_NETWORK_MARKERS)) {
            return true;
        }

        if (typeof current === 'string' && hasMarker(current, TRANSIENT_NETWORK_MARKERS)) {
            return true;
        }

        current = prop(current, 'cause');
    }

    return false;
}

/** §11.3 — a chunk that did not load is a WARN. */
export function isChunkLoadFailure(err: unknown): boolean {
    return hasMarker(typeof err === 'string' ? err : prop(err, 'message'), CHUNK_LOAD_MARKERS);
}

// ── The report path ────────────────────────────────────────────────────────────────────────────

function lastResort(message: string, err: unknown): void {
    if (state.consoleFallbackUsed) {
        return;
    }

    state.consoleFallbackUsed = true;

    try {
        // R11: the only place left to say the file itself failed.
        console.error(`[errfile] ${message}: ${safeString(err)}`);
    } catch {
        // silence is the last fallback
    }
}

function echo(record: ErrorRecord): void {
    try {
        const line = formatRecord(record);

        if (record.level === 'WARN' || record.level === 'EXPECTED') {
            console.warn(line);
        } else {
            console.error(line);
        }
    } catch {
        // echo is a convenience; never let it matter
    }
}

function deliver(s: State, record: ErrorRecord): void {
    const sink = s.sink;

    if (!sink) {
        if (s.queue.length < PRE_INSTALL_CAP) {
            s.queue.push(record);
        } else {
            s.overflow += 1;
        }

        return;
    }

    if (!record.app) {
        record.app = sink.app;
    }

    try {
        sink.write(record);
    } catch (e) {
        lastResort('the error sink threw', e);
    }

    if (sink.echo) {
        echo(record);
    }
}

function isMarkable(err: unknown): err is object {
    return (typeof err === 'object' && err !== null) || typeof err === 'function';
}

/** Has this exact error object already been written (R4)? */
export function isReported(err: unknown): boolean {
    try {
        return isMarkable(err) && state.reported.has(err);
    } catch {
        return false;
    }
}

function isVerbose(): boolean {
    return state.sink?.verbose === true || readEnv('EZBK_ERROR_FILE_VERBOSE') === '1';
}

type ReportArgs = {
    level: ErrorLevel;
    where: string;
    doing: string;
    err: unknown;
    /** False for a WARN with no error, or a `caught` handed `undefined` by the logger net. */
    hasErr: boolean;
    data: unknown;
};

function report(args: ReportArgs): void {
    const s = state;

    // Re-entrancy: anything thrown or reported WHILE reporting is dropped, so the library can never
    // loop (R11) — a throwing getter on the error, a sink that reports, an echo that faults.
    if (s.busy) {
        return;
    }

    s.busy = true;

    try {
        const { where, doing, err, hasErr } = args;
        let level = args.level;

        if (hasErr && (level === 'ERROR' || level === 'WARN') && isAnswer(err)) {
            // §11.4: an answer is not a fault. Under verbose it is still visible as EXPECTED.
            if (!isVerbose()) {
                return;
            }

            level = 'EXPECTED';
        }

        if (hasErr && isMarkable(err)) {
            if (s.reported.has(err)) {
                return;
            }

            s.reported.add(err);
        }

        let windowMs = FOLD_WINDOW_MS;
        const described: DescribedError | null = hasErr ? describeOne(err) : null;

        if (level === 'ERROR' && hasErr) {
            if (isTransientNetworkError(err)) {
                level = 'WARN';
                windowMs = TRANSIENT_WINDOW_MS;
            } else if (isChunkLoadFailure(err)) {
                level = 'WARN';
            }
        }

        const app = s.sink?.app ?? '';
        const headline = described ? redactText(headlineText(described)) : '';

        if (level !== 'FATAL') {
            const key = [app, where, doing, described?.type ?? '', normalizeMessage(described?.message ?? '')].join('\u0001');
            const context: FoldContext = { level, app, where, doing, headline };

            if (!s.folder.admit(key, windowMs, context)) {
                return;
            }
        }

        deliver(s, {
            ts: new Date().toISOString(),
            level,
            app,
            where,
            doing,
            error: headline,
            cause: hasErr ? redactText(causesText(describeChain(err))) : '',
            stack: hasErr ? redactText(stackOf(err)) : '',
            data: redactData(args.data)
        });
    } catch (e) {
        lastResort('could not report an error', e);
    } finally {
        s.busy = false;
    }
}

/** Install (or with null, remove) the sink, and drain the pre-install queue into it. */
export function setErrorSink(sink: ErrorSink | null): void {
    try {
        const s = state;
        s.sink = sink;

        if (!sink) {
            return;
        }

        const queued = s.queue;
        const overflow = s.overflow;
        s.queue = [];
        s.overflow = 0;

        for (const record of queued) {
            deliver(s, record);
        }

        if (overflow > 0) {
            deliver(s, {
                ts: new Date().toISOString(),
                level: 'WARN',
                app: sink.app,
                where: LIBRARY_WHERE,
                doing: 'installing the error file',
                error: `${overflow} records were dropped before the error file was installed`,
                cause: '',
                stack: '',
                data: null
            });
        }
    } catch (e) {
        lastResort('could not install the error sink', e);
    }
}

/** Is a sink installed in this process? */
export function hasErrorSink(): boolean {
    return state.sink !== null;
}

/** Write every owed fold summary, then flush the sink (a synchronous barrier in node). */
export function flushErrorFile(): void {
    try {
        state.folder.flushAll();
        state.sink?.flush();
    } catch (e) {
        lastResort('could not flush the error file', e);
    }
}

/** Test seam: forget the sink, the queue, the fold table and the reported set. */
export function resetErrorFileForTests(): void {
    state.folder.reset();
    state.sink = null;
    state.queue = [];
    state.overflow = 0;
    state.reported = new WeakSet<object>();
    state.busy = false;
    state.consoleFallbackUsed = false;
}

/** Headline plus cause chain, redacted — for code that must SHOW a message (a toast, a CLI line). */
export function describeError(err: unknown): string {
    try {
        return redactText(describeThrowable(err));
    } catch {
        return 'Thrown: [unprintable value]';
    }
}

/** One object per module, created at module scope: `const errors = errorFileFor('<path>')`. */
export function errorFileFor(where: string): ErrorFile {
    return {
        where,
        caught(doing, err, data) {
            report({ level: 'ERROR', where, doing, err, hasErr: err !== undefined, data });
        },
        warn(doing, err, data) {
            report({ level: 'WARN', where, doing, err, hasErr: err !== undefined, data });
        },
        expected(doing, err) {
            if (!isVerbose()) {
                return;
            }

            report({ level: 'EXPECTED', where, doing, err, hasErr: true, data: null });
        },
        rethrow(doing, err, data) {
            report({ level: 'ERROR', where, doing, err, hasErr: true, data });
            throw err;
        },
        fatal(doing, err, data) {
            report({ level: 'FATAL', where, doing, err, hasErr: true, data });
            flushErrorFile();
        }
    };
}

// ── The wrappers (§6.2, T4–T6) ─────────────────────────────────────────────────────────────────

/** Run `fn`; on a throw, report it and return `fallback`. */
export function tryOr<T>(errors: ErrorFile, doing: string, fn: () => T, fallback: T): T {
    try {
        return fn();
    } catch (e) {
        errors.caught(doing, e);
        return fallback;
    }
}

/** Await `fn()`; on a rejection, report it and resolve to `fallback`. */
export async function tryOrAsync<T>(errors: ErrorFile, doing: string, fn: () => Promise<T>, fallback: T): Promise<T> {
    try {
        return await fn();
    } catch (e) {
        errors.caught(doing, e);
        return fallback;
    }
}

function isThenable(value: unknown): value is PromiseLike<unknown> {
    return (typeof value === 'object' || typeof value === 'function') && value !== null && typeof prop(value, 'then') === 'function';
}

/** A fire-and-forget promise: report its rejection instead of losing it. */
export function reportRejection(errors: ErrorFile, doing: string, p: unknown): void {
    if (!isThenable(p)) {
        return;
    }

    try {
        p.then(undefined, (e: unknown) => errors.caught(doing, e));
    } catch (e) {
        errors.caught(doing, e);
    }
}

/**
 * Wrap a callback nobody awaits (timer, listener, onmessage). Reports a synchronous throw AND a
 * rejected promise the callback returns. Allocates once, at wrap time — never call it in render.
 */
export function guard<A extends unknown[]>(errors: ErrorFile, doing: string, fn: (...a: A) => unknown): (...a: A) => void {
    return (...a: A) => {
        try {
            const result = fn(...a);

            if (isThenable(result)) {
                reportRejection(errors, doing, result);
            }
        } catch (e) {
            errors.caught(doing, e);
        }
    };
}
