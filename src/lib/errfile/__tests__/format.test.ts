import { describe, expect, it } from 'vitest';

import { capMessage, capMiddle, describeThrowable, stripControlChars, trimStack } from '../describe.js';
import { formatRecord, summaryText, windowText } from '../format.js';
import type { ErrorRecord } from '../format.js';

function record(overrides: Partial<ErrorRecord> = {}): ErrorRecord {
    return {
        ts: '2026-09-21T16:04:11.902Z',
        level: 'ERROR',
        app: 'web',
        where: 'src/stores/transaction.ts',
        doing: 'loading the transaction list',
        error: 'TypeError: Cannot read properties of undefined (reading \'id\')',
        cause: ' | cause: AxiosError: Request failed (code=ERR_BAD_RESPONSE)',
        stack: '    at loadTransactions (src/stores/transaction.ts:412:19)',
        data: { edition: 'desktop' },
        ...overrides
    };
}

describe('format (§3.2)', () => {
    it('renders the line shape: header, data, cause, then indented stack lines', () => {
        expect(formatRecord(record())).toBe(
            '[2026-09-21T16:04:11.902Z] [ERROR] [web] [src/stores/transaction.ts] loading the transaction list — TypeError: Cannot read properties of undefined (reading \'id\') {edition=desktop} | cause: AxiosError: Request failed (code=ERR_BAD_RESPONSE)\n' +
            '    at loadTransactions (src/stores/transaction.ts:412:19)'
        );
    });

    it('walks the cause chain, codes appended', () => {
        const root = Object.assign(new Error('ECONNRESET'), { code: 'ECONNRESET' });
        const mid = new Error('socket hang up', { cause: root });
        const err = Object.assign(new Error('Request failed with status code 500', { cause: mid }), { code: 'ERR_BAD_RESPONSE' });
        err.name = 'AxiosError';
        expect(describeThrowable(err)).toBe(
            'AxiosError: Request failed with status code 500 (code=ERR_BAD_RESPONSE) | cause: Error: socket hang up | cause: Error: ECONNRESET (code=ECONNRESET)'
        );
    });

    it('stops a circular cause chain', () => {
        const a: Error & { cause?: unknown } = new Error('a');
        const b = new Error('b', { cause: a });
        a.cause = b;
        expect(describeThrowable(a)).toBe('Error: a | cause: Error: b');
    });

    it('trims the stack: drops node_modules and library frames, shortens paths, caps at 12', () => {
        const frames = [
            'Error: boom',
            '    at ours (http://localhost:8081/src/stores/account.ts:1:2)',
            '    at dep (http://localhost:8081/node_modules/.vite/deps/axios.js:3:4)',
            '    at lib (/Users/someone/repo/src/lib/errfile/core.ts:5:6)',
            '    at mcp (file:///Users/someone/repo/mcp/src/server.ts:7:8)',
            '    at abs (/Users/someone/repo/src/lib/services.ts:9:10)',
            ...Array.from({ length: 20 }, (_, i) => `    at f${i} (/x/src/b.ts:${i}:1)`)
        ].join('\n');
        const out = trimStack(frames).split('\n');
        expect(out[0]).toBe('    at ours (src/stores/account.ts:1:2)');
        expect(out[1]).toBe('    at mcp (mcp/src/server.ts:7:8)');
        expect(out[2]).toBe('    at abs (src/lib/services.ts:9:10)');
        expect(out.some(l => l.includes('node_modules'))).toBe(false);
        expect(out.some(l => l.includes('errfile'))).toBe(false);
        expect(out).toHaveLength(12);
    });

    it('a message cannot forge a second header line', () => {
        const text = formatRecord(record({
            error: 'Error: bad\n[2026-01-01T00:00:00.000Z] [ERROR] [web] forged',
            cause: '',
            stack: '',
            data: null
        }));
        expect(text.split('\n')).toHaveLength(1);
        expect(stripControlChars('a\u2028b\u2029c\x7fd')).toBe('a b c d');
    });

    it('caps the message in the middle, the data block, and the whole record', () => {
        const long = 'x'.repeat(5000) + 'END';
        const described = describeThrowable(new Error(long));
        expect(described).toContain(' …(3013 chars cut)… ');
        expect(described.endsWith('END')).toBe(true);
        expect(capMessage('y'.repeat(2000))).toHaveLength(2000);
        expect(capMiddle('z'.repeat(50), 20).length).toBeLessThanOrEqual(20);
        const data: Record<string, string> = {};

        for (let i = 0; i < 40; i++) {
            data[`k${i}`] = 'v'.repeat(100);
        }

        const line = formatRecord(record({ data, stack: '', cause: '' }));
        const block = line.slice(line.indexOf('{'), line.lastIndexOf('}') + 1);
        expect(block.length).toBeLessThanOrEqual(1002);
        const huge = formatRecord(record({ stack: Array.from({ length: 3000 }, (_, i) => `    at f${i} (x.ts:1:1)`).join('\n') }));
        expect(huge.length).toBeLessThanOrEqual(8000);
        expect(huge.startsWith('[2026-09-21T16:04:11.902Z] [ERROR]')).toBe(true);
    });

    it('describes primitives and throwing values without throwing', () => {
        expect(describeThrowable('plain string')).toBe('Thrown string: plain string');
        expect(describeThrowable(42)).toBe('Thrown number: 42');
        expect(describeThrowable(null)).toBe('Thrown: null');
        expect(describeThrowable({ foo: 1 })).toBe('Thrown: {"foo":1}');
        const hostile = {
            get message(): string {
                throw new Error('getter');
            },
            toString(): string {
                throw new Error('toString');
            }
        };
        expect(() => describeThrowable(hostile)).not.toThrow();
    });

    it('formats the folded summary line', () => {
        expect(summaryText(37, 60_000, '16:04:11', 'AxiosError: Network Error')).toBe('×37 more in the previous 60s (first at 16:04:11): AxiosError: Network Error');
        expect(windowText(600_000)).toBe('10m');
        expect(windowText(60_000)).toBe('60s');
    });
});
