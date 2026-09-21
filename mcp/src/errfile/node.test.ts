import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { errorFileFor, resetErrorFileForTests } from './core.js';
import { defaultErrorFilePath, installNodeErrorFile, resetNodeErrorFileForTests } from './node.js';
import { getStatementsRoot, setStatementsRoot } from './redact.js';

let dir = '';

beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'ezbk-errfile-node-'));
    vi.unstubAllEnvs();
});

afterEach(() => {
    resetNodeErrorFileForTests();
    resetErrorFileForTests();
    setStatementsRoot('');
    vi.unstubAllEnvs();
    rmSync(dir, { recursive: true, force: true });
});

describe('installNodeErrorFile (§5.4)', () => {
    it('under a test runner the default path is the test path, never the real file (R13)', () => {
        expect(process.env['VITEST']).toBeTruthy();
        const file = defaultErrorFilePath();
        expect(file).toMatch(/ezbookkeeping_test_\d+[\\/]error\.err$/);
        expect(file).not.toContain(join('T', 'ezbookkeeping', 'error.err'));
        expect(defaultErrorFilePath({ HOME: '/home/op' })).toBe('/home/op/T/ezbookkeeping/error.err');
        expect(defaultErrorFilePath({ NODE_ENV: 'test', TMPDIR: '/tmp' })).toMatch(/^\/tmp\/ezbookkeeping_test_\d+\/error\.err$/);
        expect(defaultErrorFilePath({ EZBK_ERROR_FILE: '/x/error.err', VITEST: 'true' })).toBe('/x/error.err');
    });

    it('returns { flush, file }, is idempotent, and flush is a synchronous barrier', () => {
        const file = join(dir, 'error.err');
        const first = installNodeErrorFile({ app: 'mcp', file, handleProcessErrors: false, echo: false });
        const second = installNodeErrorFile({ app: 'other', file: join(dir, 'ignored.err'), handleProcessErrors: false, echo: false });
        expect(first.file).toBe(file);
        expect(second.file).toBe(file);
        errorFileFor('mcp/src/server.ts').caught('running ezb_whoami', new Error('boom'), { tool: 'ezb_whoami' });
        first.flush();
        const text = readFileSync(file, 'utf8');
        expect(text).toMatch(/^\[\d{4}-\d\d-\d\dT[\d:.]+Z\] \[ERROR\] \[mcp\] \[mcp\/src\/server\.ts\] running ezb_whoami — Error: boom \{tool=ezb_whoami\}\n/);
    });

    it('echoes to stderr only, never stdout', () => {
        const file = join(dir, 'error.err');
        const stdout = vi.spyOn(process.stdout, 'write').mockImplementation(() => true);
        const stderr = vi.spyOn(process.stderr, 'write').mockImplementation(() => true);
        installNodeErrorFile({ app: 'mcp', file, handleProcessErrors: false });
        errorFileFor('mcp/src/server.ts').caught('running ezb_whoami', new Error('boom'));
        expect(stdout).not.toHaveBeenCalled();
        expect(stderr).toHaveBeenCalledTimes(1);
        expect(String(stderr.mock.calls[0]?.[0])).toContain('[ERROR] [mcp] [mcp/src/server.ts] running ezb_whoami — Error: boom');
        stdout.mockRestore();
        stderr.mockRestore();
    });

    it('EZBK_ERROR_FILE_ECHO=0 turns the echo off', () => {
        vi.stubEnv('EZBK_ERROR_FILE_ECHO', '0');
        const stderr = vi.spyOn(process.stderr, 'write').mockImplementation(() => true);
        installNodeErrorFile({ app: 'mcp', file: join(dir, 'error.err'), handleProcessErrors: false });
        errorFileFor('mcp/src/server.ts').caught('running ezb_whoami', new Error('boom'));
        expect(stderr).not.toHaveBeenCalled();
        stderr.mockRestore();
    });

    it('learns the statements root from the option or EZBK_STATEMENTS_DIR (§12.7)', () => {
        vi.stubEnv('EZBK_STATEMENTS_DIR', '/home/op/private/statements');
        const file = join(dir, 'error.err');
        const { flush } = installNodeErrorFile({ app: 'mcp', file, handleProcessErrors: false, echo: false });
        expect(getStatementsRoot()).toBe('/home/op/private/statements');
        errorFileFor('mcp/src/server.ts').caught('reading a statement', new Error('open /home/op/private/statements/2026/x.ofx: denied'));
        flush();
        const text = readFileSync(file, 'utf8');
        expect(text).toContain('<statements>/…/x.ofx');
        expect(text).not.toContain('private/statements');
    });

    it('drains the pre-install queue on install', () => {
        errorFileFor('mcp/src/config.ts').caught('reading the config before boot', new Error('early'));
        const file = join(dir, 'error.err');
        const { flush } = installNodeErrorFile({ app: 'mcp', file, handleProcessErrors: false, echo: false });
        flush();
        expect(readFileSync(file, 'utf8')).toContain('[mcp] [mcp/src/config.ts] reading the config before boot — Error: early');
    });
});
