import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
    PRE_INSTALL_CAP,
    errorFileFor,
    flushErrorFile,
    guard,
    hasErrorSink,
    isAnswer,
    isReported,
    reportRejection,
    resetErrorFileForTests,
    resolveErrorFilePath,
    setErrorSink,
    tryOr,
    tryOrAsync
} from '../core.js';
import { describeError } from '../index.js';
import { memorySink } from './helpers.js';

const errors = errorFileFor('src/lib/errfile/__tests__/core.test.ts');

beforeEach(() => {
    resetErrorFileForTests();
    vi.unstubAllEnvs();
});

afterEach(() => {
    vi.unstubAllEnvs();
});

describe('where (R14)', () => {
    it('the literal passes through unchanged', () => {
        const sink = memorySink();
        setErrorSink(sink);
        errorFileFor('src/stores/account.ts').caught('loading the account list', new Error('x'));
        expect(sink.records[0]?.where).toBe('src/stores/account.ts');
        expect(sink.lines()[0]).toContain('[src/stores/account.ts] loading the account list — Error: x');
    });
});

describe('dedupe (R4)', () => {
    it('rethrow ×3, then a caught, then the window net → 1 line', () => {
        const sink = memorySink();
        setErrorSink(sink);
        const err = new Error('deep failure');
        const layer = (n: number): void => {
            try {
                if (n === 0) {
                    throw err;
                }

                layer(n - 1);
            } catch (e) {
                errors.rethrow(`running layer ${n}`, e);
            }
        };
        expect(() => layer(2)).toThrow(err);
        errors.caught('handling it again', err);
        errorFileFor('src/desktop-main.ts').caught('an uncaught error', err);
        expect(sink.records).toHaveLength(1);
        expect(sink.records[0]?.doing).toBe('running layer 0');
        expect(isReported(err)).toBe(true);
        expect(isReported(new Error('other'))).toBe(false);
    });

    it('rethrow throws the same object, not a wrapper (R5)', () => {
        setErrorSink(memorySink());
        const err = Object.assign(new Error('typed'), { response: { status: 500 } });
        let caught: unknown;

        try {
            errors.rethrow('doing a thing', err);
        } catch (e) {
            caught = e;
        }

        expect(caught).toBe(err);
    });

    it('primitive throwables cannot be marked, so the folder handles them', () => {
        const sink = memorySink();
        setErrorSink(sink);
        errors.caught('doing', 'a string thrown');
        errors.caught('doing', 'a string thrown');
        expect(sink.records).toHaveLength(1);
        expect(sink.records[0]?.error).toBe('Thrown string: a string thrown');
    });
});

describe('expected (R6)', () => {
    it('writes nothing by default', () => {
        const sink = memorySink();
        setErrorSink(sink);
        errors.expected('reading the optional file', new Error('ENOENT'));
        flushErrorFile();
        expect(sink.records).toHaveLength(0);
    });

    it('writes EXPECTED under EZBK_ERROR_FILE_VERBOSE=1', () => {
        vi.stubEnv('EZBK_ERROR_FILE_VERBOSE', '1');
        const sink = memorySink();
        setErrorSink(sink);
        errors.expected('reading the optional file', new Error('ENOENT'));
        expect(sink.records[0]?.level).toBe('EXPECTED');
    });

    it('writes EXPECTED when the sink asks for verbose', () => {
        const sink = memorySink('test', true);
        setErrorSink(sink);
        errors.expected('probing for WebAuthn support', new Error('no'));
        expect(sink.records[0]?.level).toBe('EXPECTED');
    });
});

describe('answer shapes (§11.4, R7)', () => {
    it('caught and warn on an answer shape write nothing', () => {
        const sink = memorySink();
        setErrorSink(sink);
        errors.caught('loading the account list', { message: 'Unable to add account' });
        errors.caught('loading the account list', { error: { errorCode: 201001, errorMessage: 'exists' } });
        errors.caught('loading the account list', { processed: true });
        errors.caught('loading the account list', { canceled: true });
        errors.caught('loading the account list', { isUpToDate: true });
        errors.warn('loading the account list', { errorCode: 201001, errorMessage: 'x' });
        errors.caught('loading the account list', { message: 'x', response: { status: 400 } });
        expect(sink.records).toHaveLength(0);
    });

    it('an answer shape is EXPECTED under verbose', () => {
        const sink = memorySink('test', true);
        setErrorSink(sink);
        errors.caught('loading the account list', { processed: true });
        expect(sink.records).toHaveLength(1);
        expect(sink.records[0]?.level).toBe('EXPECTED');
    });

    it('an Error, a string, a number and an object with an extra key are written', () => {
        const sink = memorySink();
        setErrorSink(sink);
        errors.caught('one', new Error('real'));
        errors.caught('two', 'a string thrown');
        errors.caught('three', 42);
        errors.caught('four', { message: 'x', foo: 1 });
        errors.caught('five', { stack: 'Error: x\n at y', message: 'x' });
        expect(sink.records.map(r => r.doing)).toEqual(['one', 'two', 'three', 'four', 'five']);
    });

    it('isAnswer covers every shape in the list and nothing else', () => {
        expect(isAnswer({ message: 'x' })).toBe(true);
        expect(isAnswer({ error: { errorCode: 1 } })).toBe(true);
        expect(isAnswer({ error: 'x' })).toBe(false);
        expect(isAnswer({})).toBe(false);
        expect(isAnswer(null)).toBe(false);
        expect(isAnswer(new Error('x'))).toBe(false);
        expect(isAnswer([1])).toBe(false);
    });
});

describe('pre-install queue', () => {
    it('holds records reported before install and writes them on install', () => {
        expect(hasErrorSink()).toBe(false);
        errors.caught('booting before the sink', new Error('early'));
        const sink = memorySink('web');
        setErrorSink(sink);
        expect(hasErrorSink()).toBe(true);
        expect(sink.records).toHaveLength(1);
        expect(sink.records[0]?.app).toBe('web');
    });

    it('counts overflow and writes one WARN about it', () => {
        for (let i = 0; i < PRE_INSTALL_CAP + 7; i++) {
            errorFileFor(`where-${i}`).caught('booting', new Error(`e${i}`));
        }

        const sink = memorySink();
        setErrorSink(sink);
        expect(sink.records).toHaveLength(PRE_INSTALL_CAP + 1);
        expect(sink.records.at(-1)?.error).toBe('7 records were dropped before the error file was installed');
        expect(sink.records.at(-1)?.where).toBe('src/lib/errfile/core.ts');
    });
});

describe('totality (R11)', () => {
    it('a throwing sink returns normally and says so once', () => {
        const sink = memorySink();
        sink.write = () => {
            throw new Error('sink broke');
        };
        setErrorSink(sink);
        const spy = vi.spyOn(console, 'error').mockImplementation(() => undefined);
        expect(() => errors.caught('doing', new Error('x'))).not.toThrow();
        expect(() => errors.caught('doing', new Error('y'))).not.toThrow();
        expect(spy).toHaveBeenCalledTimes(1);
        spy.mockRestore();
    });

    it('a throwing getter, a throwing toString and a circular data value return normally', () => {
        const sink = memorySink();
        setErrorSink(sink);
        const hostile = {
            get message(): string {
                throw new Error('getter');
            },
            toString() {
                throw new Error('nope');
            }
        };
        const circular: Record<string, unknown> = {};
        circular['self'] = circular;
        const data = circular as unknown as Record<string, string>;
        expect(() => errors.caught('doing', hostile, data)).not.toThrow();
        expect(sink.records).toHaveLength(1);
    });

    it('re-entrancy drops the record instead of looping', () => {
        const sink = memorySink();
        const inner = errorFileFor('inner');
        sink.write = record => {
            sink.records.push(record);
            inner.caught('reporting from inside the sink', new Error('loop'));
        };
        setErrorSink(sink);
        errors.caught('outer', new Error('first'));
        expect(sink.records).toHaveLength(1);
    });
});

describe('levels', () => {
    it('a transient network error is a WARN, not an ERROR (§11.2)', () => {
        const sink = memorySink();
        setErrorSink(sink);
        errors.caught('loading the exchange rates', Object.assign(new Error('Network Error'), { name: 'AxiosError', code: 'ERR_NETWORK' }));
        errors.caught('probing the server', new Error('connect ECONNREFUSED 127.0.0.1:8080'));
        expect(sink.records.map(r => r.level)).toEqual(['WARN', 'WARN']);
    });

    it('a chunk-load failure is a WARN (§11.3)', () => {
        const sink = memorySink();
        setErrorSink(sink);
        errors.caught('navigating', new TypeError('Failed to fetch dynamically imported module: http://localhost:8081/src/views/x.vue'));
        expect(sink.records[0]?.level).toBe('WARN');
    });

    it('warn without an error is a standing condition; caught with undefined has no error text', () => {
        const sink = memorySink();
        setErrorSink(sink);
        errors.warn('the token is about to expire');
        errors.caught('failed to load settings', undefined);
        expect(sink.lines()[0]).toMatch(/\[WARN\] \[test\] \[src\/lib\/errfile\/__tests__\/core.test.ts\] the token is about to expire$/);
        expect(sink.lines()[1]).toMatch(/\[ERROR\] .* failed to load settings$/);
    });

    it('fatal writes FATAL and flushes the sink', () => {
        const sink = memorySink();
        const flush = vi.fn();
        sink.flush = flush;
        setErrorSink(sink);
        errors.fatal('starting the MCP server', new Error('boot'));
        expect(sink.records[0]?.level).toBe('FATAL');
        expect(flush).toHaveBeenCalledTimes(1);
    });
});

describe('data (§12)', () => {
    it('redacts secrets, refuses ledger fields and keeps ids', () => {
        const sink = memorySink();
        setErrorSink(sink);
        errors.caught('saving the account', new Error('x'), { accountId: '42', token: 'SYN', payee: 'SYN', count: 3 });
        expect(sink.lines()[0]).toContain('{accountId=42 token=[redacted] payee=[ledger-field refused] count=3}');
    });
});

describe('wrappers', () => {
    it('tryOr returns the fallback and reports', () => {
        const sink = memorySink();
        setErrorSink(sink);
        expect(tryOr(errors, 'parsing the saved filter', () => JSON.parse('{'), null)).toBe(null);
        expect(tryOr(errors, 'parsing', () => 5, 0)).toBe(5);
        expect(sink.records).toHaveLength(1);
    });

    it('tryOrAsync resolves the fallback and reports', async () => {
        const sink = memorySink();
        setErrorSink(sink);
        await expect(tryOrAsync(errors, 'fetching rates', () => Promise.reject(new Error('no')), [])).resolves.toEqual([]);
        expect(sink.records).toHaveLength(1);
    });

    it('reportRejection reports a fire-and-forget rejection and ignores non-promises', async () => {
        const sink = memorySink();
        setErrorSink(sink);
        reportRejection(errors, 'refreshing the user profile', Promise.reject(new Error('gone')));
        reportRejection(errors, 'nothing', undefined);
        await new Promise(r => setTimeout(r, 0));
        expect(sink.records).toHaveLength(1);
        expect(sink.records[0]?.doing).toBe('refreshing the user profile');
    });

    it('guard reports sync throws and async rejections and returns void', async () => {
        const sink = memorySink();
        setErrorSink(sink);
        const sync = guard(errors, 'handling a click', () => {
            throw new Error('sync');
        });
        const async = guard(errors, 'handling a message', async () => {
            throw new Error('async');
        });
        expect(sync()).toBeUndefined();
        expect(async()).toBeUndefined();
        await new Promise(r => setTimeout(r, 0));
        expect(sink.records.map(r => r.error)).toEqual(['Error: sync', 'Error: async']);
    });
});

describe('describeError', () => {
    it('gives the headline plus causes, redacted', () => {
        const err = new Error('fetch https://h.example/?token=SYN failed', { cause: new Error('inner') });
        expect(describeError(err)).toBe('Error: fetch https://h.example/?token=[redacted] failed | cause: Error: inner');
    });
});

describe('resolveErrorFilePath (§3.1)', () => {
    it('follows the table in order', () => {
        expect(resolveErrorFilePath({ HOME: '/home/op' }, { test: false })).toBe('/home/op/T/ezbookkeeping/error.err');
        expect(resolveErrorFilePath({ HOME: '/home/op', EZBK_ERROR_FILE: '/x/error.err' }, { test: true })).toBe('/x/error.err');
        expect(resolveErrorFilePath({ HOME: '/home/op', VITEST: 'true', TMPDIR: '/tmp' }, { uid: 7 })).toBe('/tmp/ezbookkeeping_test_7/error.err');
        expect(resolveErrorFilePath({ NODE_ENV: 'test' }, { uid: 7, tmpdir: '/var/tmp' })).toBe('/var/tmp/ezbookkeeping_test_7/error.err');
        expect(resolveErrorFilePath({}, { test: false, uid: 7, tmpdir: '/tmp/' })).toBe('/tmp/ezbookkeeping_7/error.err');
        expect(resolveErrorFilePath({}, { test: false, home: '/Users/op/' })).toBe('/Users/op/T/ezbookkeeping/error.err');
    });
});
