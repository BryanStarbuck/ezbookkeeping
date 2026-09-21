// §15.4 — the cross-language behaviour vectors. pkg/errfile/vectors_test.go runs the same file.
import fs from 'fs';
import path from 'path';
import { describe, expect, it } from 'vitest';

import { installBrowserErrorFile, resetBrowserErrorFileForTests } from '../browser.js';
import type { BrowserScope } from '../browser.js';
import {
    ANSWER_KEYS,
    CHUNK_LOAD_MARKERS,
    TRANSIENT_NETWORK_MARKERS,
    errorFileFor,
    flushErrorFile,
    isAnswer,
    isChunkLoadFailure,
    isTransientNetworkError,
    resetErrorFileForTests,
    resolveErrorFilePath,
    setErrorSink
} from '../core.js';
import {
    CAUSE_DEPTH,
    DATA_CAP,
    MESSAGE_CAP,
    MESSAGE_HEAD,
    MESSAGE_TAIL,
    RECORD_CAP,
    STACK_FRAMES,
    causesText,
    headlineText,
    stackText,
    stripControlChars
} from '../describe.js';
import type { DescribedError } from '../describe.js';
import { FOLD_KEY_MESSAGE_CAP, FOLD_MAX_KEYS, FOLD_WINDOW_MS, TRANSIENT_WINDOW_MS, normalizeMessage } from '../fold.js';
import { formatRecord, summaryText } from '../format.js';
import type { ErrorLevel, ErrorRecord, RedactedData } from '../format.js';
import { LEDGER_KEY, SECRET_KEY, URL_QUERY_REDACT_PARAMS, redactData, redactUrls, rewriteStatementsPaths, setStatementsRoot } from '../redact.js';
import { memorySink } from './helpers.js';

type VectorError = { type: string; message: string; codes: [string, string][]; cause?: VectorError[] };
type VectorRecord = {
    ts: string;
    level: ErrorLevel;
    app: string;
    where: string;
    doing: string;
    error: VectorError;
    data: [string, string][];
    stack: string[];
    fold?: { count: number; windowSeconds: number; firstAt: string };
};
type Vectors = {
    paths: { name: string; env: Record<string, string>; test: boolean; uid?: number; expect: string }[];
    format: { name: string; record: VectorRecord; expect: string }[];
    caps: Record<string, number | string>;
    control_chars: { input: string; expect: string }[];
    redact: { name: string; data: Record<string, unknown>; expect: Record<string, unknown> }[];
    redact_urls: { input: string; expect: string }[];
    statements_root: { root: string; input: string; expect: string }[];
    fold_keys: { message: string; expect: string }[];
    fold_key_parts: string[];
    fold_windows: { default_seconds: number; transient_network_seconds: number; max_keys: number };
    transient_network_markers: string[];
    answer_shapes_ts: { value: unknown; answer: boolean }[];
    benign_browser_messages: string[];
    warn_not_error_browser_messages: string[];
    normalise: { steps: { name: string; regex: string; replace: string }[] };
    control_chars_regex: string;
    secret_key_regex: string;
    ledger_key_regex: string;
    url_query_redact_params: string[];
};

const vectorsPath = path.resolve(__dirname, '../../../../pkg/errfile/testdata/vectors.json');
const vectors = JSON.parse(fs.readFileSync(vectorsPath, 'utf8')) as Vectors;

function toDescribed(e: VectorError): DescribedError {
    return { type: e.type, message: e.message, codes: e.codes };
}

function toRecord(r: VectorRecord): ErrorRecord {
    const headline = headlineText(toDescribed(r.error));
    const data: RedactedData = {};

    for (const [k, v] of r.data) {
        data[k] = v;
    }

    return {
        ts: r.ts,
        level: r.level,
        app: r.app,
        where: r.where,
        doing: r.doing,
        error: r.fold ? summaryText(r.fold.count, r.fold.windowSeconds * 1000, r.fold.firstAt, headline) : headline,
        cause: r.fold ? '' : causesText((r.error.cause ?? []).map(toDescribed)),
        stack: stackText(r.stack),
        data: r.data.length ? data : null
    };
}

describe('vectors: paths (§3.1)', () => {
    for (const v of vectors.paths) {
        it(v.name, () => {
            expect(resolveErrorFilePath(v.env, { test: v.test, uid: v.uid })).toBe(v.expect);
        });
    }
});

describe('vectors: format (§3.2)', () => {
    for (const v of vectors.format) {
        it(v.name, () => {
            expect(formatRecord(toRecord(v.record))).toBe(v.expect);
        });
    }
});

describe('vectors: caps', () => {
    it('match the constants', () => {
        expect(MESSAGE_CAP).toBe(vectors.caps['message']);
        expect(MESSAGE_HEAD).toBe(vectors.caps['message_head']);
        expect(MESSAGE_TAIL).toBe(vectors.caps['message_tail']);
        expect(DATA_CAP).toBe(vectors.caps['data']);
        expect(RECORD_CAP).toBe(vectors.caps['record']);
        expect(CAUSE_DEPTH).toBe(vectors.caps['cause_depth']);
        expect(STACK_FRAMES).toBe(vectors.caps['stack_frames']);
        expect(FOLD_KEY_MESSAGE_CAP).toBe(vectors.caps['fold_key_message']);
        const marker = String(vectors.caps['message_cut_marker']);
        const over = headlineText({ type: 'Error', message: 'x'.repeat(2500), codes: [] });
        expect(over).toContain(marker.replace('N', String(2500 - MESSAGE_HEAD - MESSAGE_TAIL)));
    });
});

describe('vectors: control characters', () => {
    for (const v of vectors.control_chars) {
        it(JSON.stringify(v.input), () => {
            expect(stripControlChars(v.input)).toBe(v.expect);
            expect(v.input.replace(new RegExp(vectors.control_chars_regex, 'g'), ' ')).toBe(v.expect);
        });
    }
});

describe('vectors: redact (§12)', () => {
    for (const v of vectors.redact) {
        it(v.name, () => {
            expect(redactData(v.data)).toEqual(v.expect);
        });
    }

    it('the key regexes match the vector file', () => {
        expect(SECRET_KEY.source).toBe(vectors.secret_key_regex.replace('(?i)', ''));
        expect(SECRET_KEY.flags).toContain('i');
        expect(LEDGER_KEY.source).toBe(vectors.ledger_key_regex.replace('(?i)', ''));
        expect(LEDGER_KEY.flags).toContain('i');
        expect([...URL_QUERY_REDACT_PARAMS]).toEqual(vectors.url_query_redact_params);
    });

    for (const v of vectors.redact_urls) {
        it(`url: ${v.input}`, () => {
            expect(redactUrls(v.input)).toBe(v.expect);
        });
    }

    for (const v of vectors.statements_root) {
        it(`statements root ${JSON.stringify(v.root)}: ${v.input}`, () => {
            setStatementsRoot(v.root);

            try {
                expect(rewriteStatementsPaths(v.input)).toBe(v.expect);
            } finally {
                setStatementsRoot('');
            }
        });
    }
});

describe('vectors: fold keys (§11.1)', () => {
    for (const v of vectors.fold_keys) {
        it(JSON.stringify(v.message.slice(0, 60)), () => {
            expect(normalizeMessage(v.message)).toBe(v.expect);
        });
    }

    it('the normalise steps, applied in order with the vector regexes, give the same result', () => {
        for (const v of vectors.fold_keys) {
            let out = v.message;

            for (const step of vectors.normalise.steps) {
                out = out.replace(new RegExp(step.regex, 'g'), step.replace);
            }

            expect(out.slice(0, FOLD_KEY_MESSAGE_CAP)).toBe(v.expect);
        }
    });

    it('the windows and the key cap match', () => {
        expect(FOLD_WINDOW_MS).toBe(vectors.fold_windows.default_seconds * 1000);
        expect(TRANSIENT_WINDOW_MS).toBe(vectors.fold_windows.transient_network_seconds * 1000);
        expect(FOLD_MAX_KEYS).toBe(vectors.fold_windows.max_keys);
        expect(vectors.fold_key_parts).toEqual(['app', 'where_file', 'doing', 'error_type', 'normalised_message']);
    });

    it('the key is app + where + doing + type + normalised message', () => {
        resetErrorFileForTests();
        const sink = memorySink('web');
        setErrorSink(sink);
        const errors = errorFileFor('src/x.ts');
        errors.caught('doing', new Error('id 123 failed'));
        errors.caught('doing', new Error('id 456 failed'));
        errors.caught('doing', new TypeError('id 789 failed'));
        expect(sink.records).toHaveLength(2);
        resetErrorFileForTests();
    });
});

describe('vectors: transient network markers (§11.2)', () => {
    it('match the constants', () => {
        expect([...TRANSIENT_NETWORK_MARKERS]).toEqual(vectors.transient_network_markers);
    });

    for (const marker of vectors.transient_network_markers) {
        it(`${marker} as a code or a message is transient`, () => {
            expect(isTransientNetworkError(Object.assign(new Error('x'), { code: marker }))).toBe(true);
            expect(isTransientNetworkError(new Error(`failed: ${marker}`))).toBe(true);
            expect(isTransientNetworkError(new Error('outer', { cause: new Error(marker) }))).toBe(true);
        });
    }

    it('an ordinary error is not', () => {
        expect(isTransientNetworkError(new Error('boom'))).toBe(false);
    });
});

describe('vectors: answer shapes (§11.4)', () => {
    it('the key list matches', () => {
        expect([...ANSWER_KEYS]).toEqual(['message', 'error', 'processed', 'canceled', 'isUpToDate', 'errorCode', 'errorMessage', 'response']);
    });

    for (const v of vectors.answer_shapes_ts) {
        it(`${JSON.stringify(v.value)} → ${v.answer}`, () => {
            expect(isAnswer(v.value)).toBe(v.answer);
            resetErrorFileForTests();
            const sink = memorySink('web');
            setErrorSink(sink);
            errorFileFor('src/x.ts').caught('loading', v.value);
            expect(sink.records).toHaveLength(v.answer ? 0 : 1);
            resetErrorFileForTests();
        });
    }
});

describe('vectors: browser messages (§11.3)', () => {
    for (const message of vectors.benign_browser_messages) {
        it(`benign without an Error object: ${message}`, () => {
            resetBrowserErrorFileForTests();
            resetErrorFileForTests();
            const posts: string[] = [];
            const listeners = new Map<string, (event: unknown) => void>();
            const scope: BrowserScope = {
                addEventListener: (type, listener) => listeners.set(type, listener),
                fetch: (_url, init) => {
                    posts.push(String(init.body));
                    return Promise.resolve({});
                }
            };
            installBrowserErrorFile({ app: 'web', scope });
            listeners.get('error')?.({ error: null, message });
            listeners.get('error')?.({ error: new Error('real one'), message: 'real one' });
            flushErrorFile();
            resetBrowserErrorFileForTests();
            expect(posts).toHaveLength(1);
            expect(posts[0]).toContain('real one');
            expect(posts[0]).not.toContain(message);
        });
    }

    it('chunk-load messages are WARN, not ERROR', () => {
        expect([...CHUNK_LOAD_MARKERS]).toEqual(vectors.warn_not_error_browser_messages);

        for (const message of vectors.warn_not_error_browser_messages) {
            expect(isChunkLoadFailure(new TypeError(`${message}: http://localhost/x.js`))).toBe(true);
        }
    });
});
