import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { errorFileFor, resetErrorFileForTests, setErrorSink } from '../core.js';
import { FOLD_MAX_KEYS, TRANSIENT_WINDOW_MS, createBurstFolder, normalizeMessage } from '../fold.js';
import type { FoldSummary } from '../fold.js';
import { memorySink } from './helpers.js';

const errors = errorFileFor('src/lib/errfile/__tests__/fold.test.ts');

beforeEach(() => resetErrorFileForTests());
afterEach(() => vi.useRealTimers());

describe('fold (R10, M6)', () => {
    it('500 identical faults in 10 s → 1 line + 1 summary', () => {
        vi.useFakeTimers();
        const sink = memorySink();
        setErrorSink(sink);

        for (let i = 0; i < 500; i++) {
            errors.caught('loading the transaction list', new Error(`timeout after ${i} ms`));
            vi.advanceTimersByTime(20);
        }

        expect(sink.records).toHaveLength(1);
        vi.advanceTimersByTime(61_000);
        expect(sink.records).toHaveLength(2);
        expect(sink.records[1]?.error).toMatch(/^×499 more in the previous 60s \(first at \d\d:\d\d:\d\d\): Error: timeout after 499 ms$/);
        expect(sink.records[1]?.where).toBe('src/lib/errfile/__tests__/fold.test.ts');
    });

    it('a transient network burst folds per 10 minutes as WARN (§11.2)', () => {
        vi.useFakeTimers();
        const sink = memorySink();
        setErrorSink(sink);

        for (let i = 0; i < 9; i++) {
            errors.caught('fetching the exchange rates', Object.assign(new Error('Network Error'), { code: 'ERR_NETWORK' }));
            vi.advanceTimersByTime(60_000);
        }

        expect(sink.records).toHaveLength(1);
        expect(sink.records[0]?.level).toBe('WARN');
        vi.advanceTimersByTime(TRANSIENT_WINDOW_MS);
        expect(sink.records).toHaveLength(2);
        expect(sink.records[1]?.error).toMatch(/^×8 more in the previous 10m/);
    });

    it('normalises digits, hex, uuids and paths so `id 123` folds with `id 456`', () => {
        expect(normalizeMessage('id 123')).toBe(normalizeMessage('id 456'));
        expect(normalizeMessage('blob deadbeefcafe0001')).toBe('blob <hex>');
        expect(normalizeMessage('row 0b6e5d9a-2a8a-4d4b-9f5e-6a0c7d1f2e3b')).toBe('row <uuid>');
        expect(normalizeMessage('open /Users/x/a.db failed')).toBe('open <path> failed');
        expect(normalizeMessage('x'.repeat(400))).toHaveLength(300);
    });

    it('the key cap evicts least-recently-seen and still writes owed summaries', () => {
        const summaries: FoldSummary[] = [];
        const folder = createBurstFolder({ maxKeys: 2, emit: s => summaries.push(s) });
        expect(folder.admit('a', 60_000, null, 0)).toBe(true);
        expect(folder.admit('a', 60_000, null, 1)).toBe(false); // a owes 1
        expect(folder.admit('b', 60_000, null, 2)).toBe(true);
        expect(folder.admit('c', 60_000, null, 3)).toBe(true); // evicts a (least recently seen)
        expect(folder.size()).toBe(2);
        expect(summaries).toEqual([{ key: 'a', count: 1, windowMs: 60_000, firstAt: 0 }]);
        folder.reset();
        expect(FOLD_MAX_KEYS).toBe(1000);
    });

    it('flushAll writes every owed summary (exit)', () => {
        const summaries: FoldSummary[] = [];
        const folder = createBurstFolder({ maxKeys: 10, emit: s => summaries.push(s) });
        folder.admit('k', 60_000, null, 0);
        folder.admit('k', 60_000, null, 5);
        folder.admit('k', 60_000, null, 6);
        folder.flushAll();
        expect(summaries[0]?.count).toBe(2);
        folder.reset();
    });

    it('the stack is never part of the key; where, doing and type are', () => {
        const sink = memorySink();
        setErrorSink(sink);
        const a = new Error('same');
        a.stack = 'Error: same\n    at one (src/a.ts:1:1)';
        const b = new Error('same');
        b.stack = 'Error: same\n    at two (src/b.ts:2:2)';
        errors.caught('doing', a);
        errors.caught('doing', b);
        expect(sink.records).toHaveLength(1);
        errors.caught('doing something else', new Error('same'));
        errors.caught('doing', new TypeError('same'));
        errorFileFor('elsewhere').caught('doing', new Error('same'));
        expect(sink.records).toHaveLength(4);
    });
});
