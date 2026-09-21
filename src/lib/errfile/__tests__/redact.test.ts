import { afterEach, describe, expect, it } from 'vitest';

import { errorFileFor, resetErrorFileForTests, setErrorSink } from '../core.js';
import { LEDGER_REFUSED, REDACTED, redactData, redactUrls, rewriteStatementsPaths, setStatementsRoot } from '../redact.js';
import { memorySink } from './helpers.js';

// Synthetic corpus only — never a real value (§12.6).
const SECRET_KEYS = ['password', 'pass', 'clientSecret', 'accessToken', 'Authorization', 'cookie', 'sessionId', 'apiKey', 'X-Ezbk-Api-Key', 'signature', 'credentials', 'bearer', 'totp', 'otp', 'passcode'];
const LEDGER_KEYS = ['amount', 'balance', 'payee', 'payeeName', 'comment', 'notes', 'note', 'memo', 'account_name', 'accountName', 'category_name', 'categoryName', 'tag_name', 'description', 'imported_payee', 'statementText', 'iban', 'cardNumber', 'number'];
const SYNTHETIC = 'SYNTHETIC-VALUE-9f3a';

afterEach(() => {
    setStatementsRoot('');
    resetErrorFileForTests();
});

describe('redact (§12, M10)', () => {
    it('redacts every secret key', () => {
        const out = redactData(Object.fromEntries(SECRET_KEYS.map(k => [k, SYNTHETIC])));

        for (const key of SECRET_KEYS) {
            expect(out?.[key], key).toBe(REDACTED);
        }
    });

    it('refuses every ledger key', () => {
        const out = redactData(Object.fromEntries(LEDGER_KEYS.map(k => [k, SYNTHETIC])));

        for (const key of LEDGER_KEYS) {
            expect(out?.[key], key).toBe(LEDGER_REFUSED);
        }
    });

    it('lets opaque ids, counts, booleans and null through with their types', () => {
        expect(redactData({ account_id: '8790153498210304', count: 3, ok: true, nil: null, gone: undefined })).toEqual({
            account_id: '8790153498210304',
            count: 3,
            ok: true,
            nil: null
        });
        expect(redactData({ obj: { a: 1 }, arr: [1], fn: () => 1 })).toEqual({ obj: '[object]', arr: '[array]', fn: '[function]' });
        expect(redactData(null)).toBe(null);
        expect(redactData('text')).toBe(null);
        expect(redactData({})).toBe(null);
    });

    it('cleans keys and caps the number of pairs', () => {
        const data: Record<string, string> = {};

        for (let i = 0; i < 50; i++) {
            data[`k ${i}=x`] = 'v';
        }

        const out = redactData(data);
        expect(Object.keys(out ?? {})).toHaveLength(40);
        expect(out?.['k_0_x']).toBe('v');
    });

    it('redacts URL query values but keeps the names', () => {
        expect(redactUrls('GET https://bank.example/cb?code=abc&state=xyz&page=2&sig=zz#frag failed')).toBe(
            'GET https://bank.example/cb?code=[redacted]&state=[redacted]&page=2&sig=[redacted]#frag failed'
        );
        expect(redactUrls('https://rates.example.com/latest?api_key=k123&base=USD')).toBe('https://rates.example.com/latest?api_key=[redacted]&base=USD');
        expect(redactUrls('no url here')).toBe('no url here');
    });

    it('rewrites a statements-root path and leaves other paths alone (§12.7)', () => {
        setStatementsRoot('/home/operator/private/bank_statements/');
        expect(rewriteStatementsPaths('open /home/operator/private/bank_statements/2026/northbank/2026-03.ofx: no such file')).toBe('open <statements>/…/2026-03.ofx: no such file');
        expect(rewriteStatementsPaths('open /home/operator/other/file.txt: no such file')).toBe('open /home/operator/other/file.txt: no such file');
        expect(rewriteStatementsPaths('listing /home/operator/private/bank_statements')).toBe('listing <statements>');
        setStatementsRoot('');
        expect(rewriteStatementsPaths('open /home/operator/private/bank_statements/x.ofx')).toBe('open /home/operator/private/bank_statements/x.ofx');
    });

    it('no secret, ledger or statements value reaches a written line', () => {
        setStatementsRoot('/home/operator/private/bank_statements');
        const sink = memorySink();
        setErrorSink(sink);
        const data = Object.fromEntries([...SECRET_KEYS, ...LEDGER_KEYS].map(k => [k, SYNTHETIC]));
        const err = new Error(`fetch https://h.example/?token=${SYNTHETIC} failed`, {
            cause: new Error(`open /home/operator/private/bank_statements/${SYNTHETIC}/2026-03.ofx: denied`)
        });
        err.stack = `Error: x\n    at f (/home/operator/private/bank_statements/${SYNTHETIC}/x.ts:1:1)`;
        errorFileFor('x').caught('posting a synthetic request', err, data);
        const text = sink.lines().join('');
        expect(text).not.toContain(SYNTHETIC);
        expect(text).not.toContain('bank_statements');
        expect(text).toContain('<statements>/…/2026-03.ofx');
    });
});
