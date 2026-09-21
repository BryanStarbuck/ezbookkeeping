// Turning a thrown value into text — pm/error_err.mdx §3.2.
//
// Everything here runs ONLY on the error path, and every function is total: a hostile or broken
// throwable (a throwing getter, a throwing toString, a circular cause chain) produces text, never
// an exception (R11). This file imports nothing: it is the bottom of the library.

/** The message is capped here (§3.2), cut in the middle so the start and the end both survive. */
export const MESSAGE_CAP = 2000;
/** How much of the start of an over-long message survives … */
export const MESSAGE_HEAD = 1000;
/** … and how much of the end. */
export const MESSAGE_TAIL = 990;
/** The data block is capped here. */
export const DATA_CAP = 1000;
/** The whole record (header + stack) is capped here. */
export const RECORD_CAP = 8000;
/** How far `err.cause` is walked. */
export const CAUSE_DEPTH = 5;
/** Stack frames kept after trimming. */
export const STACK_FRAMES = 12;

/** `Type: message (k=v …)` — one link of a cause chain, already turned into plain data. */
export type DescribedError = {
    type: string;
    message: string;
    codes: [string, string][];
};

// \x00-\x1f, \x7f, U+2028 and U+2029: anything that could end a line or forge a second header.
const CONTROL_CHARS = /[\u0000-\u001f\u007f\u2028\u2029]/g;

/** Replace every control character (and the two Unicode line separators) with a space. */
export function stripControlChars(value: string): string {
    return value.replace(CONTROL_CHARS, ' ');
}

/** The `⋯(N chars cut)⋯` marker put where a string was cut. */
function cutMarker(cut: number): string {
    return ` …(${cut} chars cut)… `;
}

/** Cap a message at MESSAGE_CAP: the first MESSAGE_HEAD and the last MESSAGE_TAIL characters survive. */
export function capMessage(value: string): string {
    if (value.length <= MESSAGE_CAP) {
        return value;
    }

    const cut = value.length - MESSAGE_HEAD - MESSAGE_TAIL;
    return value.slice(0, MESSAGE_HEAD) + cutMarker(cut) + value.slice(value.length - MESSAGE_TAIL);
}

/** Cap any string at `max` characters by cutting out its middle. */
export function capMiddle(value: string, max: number): string {
    if (value.length <= max) {
        return value;
    }

    const keep = Math.max(0, max - cutMarker(value.length).length);
    const head = Math.ceil(keep / 2);
    const tail = keep - head;
    const cut = value.length - keep;
    return value.slice(0, head) + cutMarker(cut) + (tail > 0 ? value.slice(value.length - tail) : '');
}

/** Read a property without letting a throwing getter escape. */
export function prop(value: unknown, key: string): unknown {
    if (value === null || (typeof value !== 'object' && typeof value !== 'function')) {
        return undefined;
    }

    try {
        return Reflect.get(value, key);
    } catch {
        return undefined;
    }
}

/** String(value), but total. */
export function safeString(value: unknown): string {
    try {
        if (typeof value === 'string') {
            return value;
        }

        if (value === undefined) {
            return 'undefined';
        }

        if (value === null) {
            return 'null';
        }

        if (typeof value === 'object') {
            try {
                const json = JSON.stringify(value);

                if (typeof json === 'string' && json !== '{}') {
                    return json;
                }
            } catch {
                // circular or throwing toJSON — fall through to String()
            }
        }

        return String(value);
    } catch {
        return '[unprintable value]';
    }
}

function isErrorLike(value: unknown): boolean {
    return value instanceof Error || (typeof value === 'object' && value !== null && typeof prop(value, 'message') === 'string');
}

/** The fields that become the `(code=… errno=…)` suffix, in order (§3.2). */
const CODE_KEYS = ['code', 'errno', 'op', 'syscall', 'HttpStatusCode', 'errorCode'];

function codesOf(err: unknown): [string, string][] {
    const codes: [string, string][] = [];

    if (typeof err !== 'object' || err === null) {
        return codes;
    }

    for (const key of CODE_KEYS) {
        const v = prop(err, key);

        if (typeof v === 'string' || typeof v === 'number') {
            codes.push([key, String(v)]);
        }
    }

    return codes;
}

/** One throwable → `{ type, message, codes }`, without its cause chain. Total. */
export function describeOne(err: unknown): DescribedError {
    try {
        if (isErrorLike(err)) {
            const rawName = prop(err, 'name');
            const rawMessage = prop(err, 'message');

            return {
                type: typeof rawName === 'string' && rawName ? rawName : 'Error',
                message: typeof rawMessage === 'string' ? rawMessage : '',
                codes: codesOf(err)
            };
        }

        return {
            type: typeof err === 'object' ? 'Thrown' : 'Thrown ' + typeof err,
            message: safeString(err),
            codes: []
        };
    } catch {
        return { type: 'Thrown', message: '[unprintable value]', codes: [] };
    }
}

/** The `.cause` chain of a throwable, outermost first, at most CAUSE_DEPTH links, cycle-safe. */
export function describeChain(err: unknown): DescribedError[] {
    const chain: DescribedError[] = [];
    const seen = new Set<unknown>([err]);
    let current = prop(err, 'cause');

    for (let depth = 0; depth < CAUSE_DEPTH && current !== undefined && current !== null; depth++) {
        if (seen.has(current)) {
            break;
        }

        seen.add(current);
        chain.push(describeOne(current));
        current = prop(current, 'cause');
    }

    return chain;
}

/** `Type: message (k=v …)` as text — the message stripped of control characters and capped. */
export function headlineText(d: DescribedError): string {
    const message = capMessage(stripControlChars(d.message));
    const head = message ? `${stripControlChars(d.type)}: ${message}` : stripControlChars(d.type);
    const codes = d.codes.map(([k, v]) => `${stripControlChars(k)}=${stripControlChars(v)}`);
    return codes.length ? `${head} (${codes.join(' ')})` : head;
}

/** ` | cause: …` for each link of a cause chain, at most CAUSE_DEPTH of them. */
export function causesText(chain: DescribedError[]): string {
    let out = '';

    for (const link of chain.slice(0, CAUSE_DEPTH)) {
        out += ` | cause: ${headlineText(link)}`;
    }

    return out;
}

/** Frames → indented `    at …` lines, at most STACK_FRAMES of them. */
export function stackText(frames: string[]): string {
    return frames.slice(0, STACK_FRAMES).map(f => `    at ${stripControlChars(f)}`).join('\n');
}

/**
 * The one-line description of a throwable: headline plus cause chain, no stack. For code that has
 * to SHOW a message — never to build a report by hand (R2). Secrets are not redacted here; use
 * `describeError` from index.ts for anything that leaves the process.
 */
export function describeThrowable(err: unknown): string {
    return headlineText(describeOne(err)) + causesText(describeChain(err));
}

// Absolute prefixes shortened to repo-relative: anything up to a `/src/`, `/mcp/src/` or
// `/mcp/dist/` segment, a file:// scheme, or a loopback origin (http://localhost:8081/).
const REPO_PREFIX = /(?:file:\/\/)?(?:\/[^\s():]*?)?\/(src\/|mcp\/(?:src|dist)\/)/g;
const ORIGIN_PREFIX = /\bhttps?:\/\/(?:localhost|127\.0\.0\.1|\[::1\]|0\.0\.0\.0)(?::\d+)?\//g;
// V8 `    at fn (file:line:col)` / `    at file:line:col`; Firefox/Safari `fn@file:line:col`.
const FRAME = /^\s*at\s|^[^\s@]*@\S+:\d+/;

function isOurOwnFrame(line: string): boolean {
    return line.includes('node_modules') ||
        line.includes('(node:internal/') ||
        line.includes(' node:internal/') ||
        line.includes('/errfile/') ||
        line.includes('/@vite/client');
}

/** Keep only frame lines, drop node_modules and our own frames, cap, shorten paths, indent. */
export function trimStack(stack: string, maxFrames: number = STACK_FRAMES): string {
    try {
        const frames: string[] = [];

        for (const raw of stack.split('\n')) {
            if (frames.length >= maxFrames) {
                break;
            }

            if (!FRAME.test(raw) || isOurOwnFrame(raw)) {
                continue;
            }

            const line = stripControlChars(raw.trim())
                .replace(ORIGIN_PREFIX, '')
                .replace(REPO_PREFIX, '$1');
            frames.push(`    ${line.startsWith('at ') ? line : 'at ' + line}`);
        }

        return frames.join('\n');
    } catch {
        return '';
    }
}

/** The trimmed stack of a throwable, or '' when it has none. */
export function stackOf(err: unknown): string {
    const stack = prop(err, 'stack');
    return typeof stack === 'string' ? trimStack(stack) : '';
}
