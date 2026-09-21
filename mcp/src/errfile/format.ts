// GENERATED from src/lib/errfile — run scripts/sync-errfile.sh
// Record → line(s) — pm/error_err.mdx §3.2.
//
//   [ts] [LEVEL] [app] [where] doing — Type: message (code=…) {k=v …} | cause: …
//       at frame
//       at frame
//
// The line has no trailing newline; the sink that writes it adds one.

import { DATA_CAP, RECORD_CAP, capMiddle, stripControlChars } from './describe.js';

export type ErrorLevel = 'WARN' | 'ERROR' | 'FATAL' | 'EXPECTED';

/** A redacted data block: allowlisted primitives only (§12). */
export type RedactedData = Record<string, string | number | boolean | null>;

/** One fault, already described and redacted. Plain data: it crosses fetch and postMessage. */
export type ErrorRecord = {
    ts: string;
    level: ErrorLevel;
    /** The runtime (§3.3). Empty until a sink stamps it. */
    app: string;
    /** Repo-relative source path (R14). */
    where: string;
    /** Gerund phrase: what was being done. */
    doing: string;
    /** `Type: message (code=…)` — '' for a WARN with no error; the summary text for a folded line. */
    error: string;
    /** ` | cause: …` chain, '' when there is none. */
    cause: string;
    /** Trimmed, indented stack lines, '' when there is none. */
    stack: string;
    /** Allowlisted, redacted primitives (§12). */
    data: RedactedData | null;
};

function field(value: string): string {
    return stripControlChars(value);
}

function valueText(value: string | number | boolean | null): string {
    return value === null ? 'null' : field(String(value));
}

/** ` {k=v k=v}`, capped at DATA_CAP; '' when there is nothing to show. */
export function formatData(data: RedactedData | null | undefined): string {
    if (!data) {
        return '';
    }

    const pairs: string[] = [];

    for (const key of Object.keys(data)) {
        const value = data[key];

        if (value === undefined) {
            continue;
        }

        pairs.push(`${field(key)}=${valueText(value)}`);
    }

    if (!pairs.length) {
        return '';
    }

    return ` {${capMiddle(pairs.join(' '), DATA_CAP)}}`;
}

/** The single header line (no trailing newline). */
export function formatHeader(record: ErrorRecord): string {
    const head = `[${field(record.ts)}] [${field(record.level)}] [${field(record.app || '?')}] [${field(record.where)}] ${field(record.doing)}`;
    const error = record.error ? ` — ${field(record.error)}` : '';
    return head + error + formatData(record.data) + field(record.cause);
}

/** The whole record: header plus indented stack lines, capped at RECORD_CAP. No trailing newline. */
export function formatRecord(record: ErrorRecord): string {
    const header = formatHeader(record);

    if (!record.stack) {
        return header.length <= RECORD_CAP ? header : capMiddle(header, RECORD_CAP);
    }

    const text = `${header}\n${record.stack}`;

    if (text.length <= RECORD_CAP) {
        return text;
    }

    // Keep the header whole if possible; drop trailing stack lines until it fits.
    if (header.length >= RECORD_CAP) {
        return capMiddle(header, RECORD_CAP);
    }

    const lines = record.stack.split('\n');
    let out = header;

    for (const line of lines) {
        if (out.length + 1 + line.length > RECORD_CAP) {
            break;
        }

        out += `\n${line}`;
    }

    return out;
}

function pad2(n: number): string {
    return n < 10 ? `0${n}` : String(n);
}

/** HH:MM:SS (UTC, like the timestamps) for the folded summary's "first at". */
export function clockTime(ms: number): string {
    const d = new Date(ms);
    return `${pad2(d.getUTCHours())}:${pad2(d.getUTCMinutes())}:${pad2(d.getUTCSeconds())}`;
}

/** `60s`, `10m` — the window as the summary line prints it. */
export function windowText(windowMs: number): string {
    if (windowMs > 60_000 && windowMs % 60_000 === 0) {
        return `${windowMs / 60_000}m`;
    }

    return `${Math.round(windowMs / 1000)}s`;
}

/** The `error` text of a folded summary line (§3.2): `×37 more in the previous 60s (first at 16:04:11): Type: message`. */
export function summaryText(count: number, windowMs: number, firstAtClock: string, headline: string): string {
    const what = headline ? `: ${headline}` : '';
    return `×${count} more in the previous ${windowText(windowMs)} (first at ${firstAtClock})${what}`;
}
