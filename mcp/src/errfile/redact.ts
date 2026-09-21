// GENERATED from src/lib/errfile — run scripts/sync-errfile.sh
// Privacy — pm/error_err.mdx §12. The library enforces this; call sites are not trusted to (R12).
//
// Applied to every `data` value, and the text rules also to messages, causes and stacks:
//   1. a key that names ledger data is DROPPED and replaced with [ledger-field refused];
//   2. a key that names a secret has its value replaced with [redacted];
//   3. a URL has the values of its sensitive query parameters replaced (names kept);
//   4. an absolute path under the statements root becomes <statements>/…/basename;
//   5. a non-primitive value becomes [object] / [array] — ledger rows hide inside objects.
// Order per pkg/errfile/testdata/vectors.json `_order_note`: ledger first, then secret.

import { capMiddle, safeString, stripControlChars } from './describe.js';
import type { RedactedData } from './format.js';

/** Keys whose VALUES are secrets. */
export const SECRET_KEY = /pass(word)?|secret|token|auth|cookie|session|key|signature|credential|bearer|totp|otp|passcode/i;

/** Keys whose values are ledger data — refused outright: the repo is public and the books are not. */
export const LEDGER_KEY = /amount|balance|payee|comment|notes?|memo|account_?name|category_?name|tag_?name|description|statement|iban|card|number/i;

export const REDACTED = '[redacted]';
export const LEDGER_REFUSED = '[ledger-field refused]';

/** Query parameters whose values are redacted inside any URL (§12.2). */
export const URL_QUERY_REDACT_PARAMS: readonly string[] = [
    'token', 'code', 'state', 'key', 'password', 'secret', 'signature', 'sig', 'passcode', 'api_key'
];

const SENSITIVE_QUERY = new Set(URL_QUERY_REDACT_PARAMS);

const URL_PATTERN = /\b[a-z][a-z0-9+.-]*:\/\/[^\s"'<>]+/gi;

function redactOneUrl(url: string): string {
    const q = url.indexOf('?');

    if (q < 0) {
        return url;
    }

    const hash = url.indexOf('#', q);
    const query = url.slice(q + 1, hash < 0 ? undefined : hash);
    const rest = hash < 0 ? '' : url.slice(hash);
    const redacted = query
        .split('&')
        .map(pair => {
            const eq = pair.indexOf('=');
            const name = eq < 0 ? pair : pair.slice(0, eq);
            let decoded = name;

            try {
                decoded = decodeURIComponent(name);
            } catch {
                // a malformed escape is still a name; compare it raw
            }

            return eq >= 0 && SENSITIVE_QUERY.has(decoded.toLowerCase()) ? `${name}=${REDACTED}` : pair;
        })
        .join('&');

    return `${url.slice(0, q)}?${redacted}${rest}`;
}

/** Redact sensitive query values in every URL found inside `text`. */
export function redactUrls(text: string): string {
    try {
        return text.includes('://') ? text.replace(URL_PATTERN, redactOneUrl) : text;
    } catch {
        return text;
    }
}

// ── The statements root (§12.7) ────────────────────────────────────────────────────────────────
// Learned from Install / EZBK_STATEMENTS_DIR, never from a constant: the directory name is private.

let statementsRoot = '';
let statementsPattern: RegExp | null = null;

function escapeRegExp(value: string): string {
    return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** Tell the library where the bank statements live, so that path is never written. '' clears it. */
export function setStatementsRoot(root: string | undefined | null): void {
    const cleaned = (root ?? '').replace(/[\\/]+$/, '');
    statementsRoot = cleaned;
    statementsPattern = cleaned ? new RegExp(`${escapeRegExp(cleaned)}(?:[\\\\/][^\\s:'"()\\\\/]+)*`, 'g') : null;
}

export function getStatementsRoot(): string {
    return statementsRoot;
}

/** Rewrite every absolute path under the statements root to `<statements>/…/basename`. */
export function rewriteStatementsPaths(text: string): string {
    if (!statementsPattern || !text.includes(statementsRoot)) {
        return text;
    }

    try {
        return text.replace(statementsPattern, match => {
            const tail = match.slice(statementsRoot.length);
            const parts = tail.split(/[\\/]/).filter(Boolean);
            const base = parts[parts.length - 1];
            return base ? `<statements>/…/${base}` : '<statements>';
        });
    } catch {
        return text;
    }
}

/** Every text rule at once: URL query values, then the statements root. */
export function redactText(text: string): string {
    return rewriteStatementsPaths(redactUrls(text));
}

// ── The data block ─────────────────────────────────────────────────────────────────────────────

/** Per-value cap inside the data block; the whole block is capped again when formatted. */
const VALUE_CAP = 300;
/** At most this many keys survive. */
const MAX_KEYS = 40;

export type ErrorDataValue = string | number | boolean | null | undefined;
export type ErrorData = Record<string, ErrorDataValue>;

/**
 * Apply §12 to one key/value. Returns `undefined` when the pair should be omitted (an undefined
 * value). Primitive types are kept; anything else becomes its type name in brackets.
 */
export function redactValue(key: string, value: unknown): string | number | boolean | null | undefined {
    if (LEDGER_KEY.test(key)) {
        return LEDGER_REFUSED;
    }

    if (value === undefined) {
        return undefined;
    }

    if (SECRET_KEY.test(key)) {
        return REDACTED;
    }

    if (value === null) {
        return null;
    }

    if (typeof value === 'number' || typeof value === 'boolean') {
        return value;
    }

    if (typeof value === 'string') {
        return capMiddle(stripControlChars(redactText(value)), VALUE_CAP);
    }

    if (Array.isArray(value)) {
        return '[array]';
    }

    if (typeof value === 'object') {
        return '[object]';
    }

    return `[${typeof value}]`;
}

/** Redact a whole data block. Accepts `unknown`: nothing about it is trusted. */
export function redactData(data: unknown): RedactedData | null {
    if (data === null || data === undefined || typeof data !== 'object') {
        return null;
    }

    try {
        const out: RedactedData = {};
        let count = 0;

        for (const key of Object.keys(data)) {
            if (count >= MAX_KEYS) {
                break;
            }

            let raw: unknown;

            try {
                raw = Reflect.get(data, key);
            } catch {
                raw = '[unreadable]';
            }

            const cleanKey = stripControlChars(key).replace(/[\s={}]/g, '_').slice(0, 60);
            const value = redactValue(cleanKey, raw);

            if (value === undefined) {
                continue;
            }

            out[cleanKey] = value;
            count++;
        }

        return count ? out : null;
    } catch {
        return { data: safeString('[unreadable data]') };
    }
}
