import * as realFs from 'node:fs';
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { errorFileFor, resetErrorFileForTests } from './core.js';
import { installNodeErrorFile, resetNodeErrorFileForTests } from './node.js';
import { RollingFileWriter } from './rolling-file-writer.js';

vi.mock('node:fs', async importOriginal => {
    const actual = await importOriginal<typeof realFs>();
    return {
        ...actual,
        appendFileSync: vi.fn(actual.appendFileSync),
        mkdirSync: vi.fn(actual.mkdirSync),
        statSync: vi.fn(actual.statSync),
        existsSync: vi.fn(actual.existsSync),
        renameSync: vi.fn(actual.renameSync)
    };
});

let dir = '';

beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'ezbk-errfile-writer-'));
    vi.clearAllMocks();
});

afterEach(() => {
    resetNodeErrorFileForTests();
    resetErrorFileForTests();
    rmSync(dir, { recursive: true, force: true });
});

/** Wait for the async drain (mkdir + stat + appendFile on the threadpool) to land. */
async function waitForDrain(file?: string): Promise<void> {
    const deadline = Date.now() + 3000;

    do {
        await new Promise(resolve => setTimeout(resolve, 20));
    } while (file && !realFs.existsSync(file) && Date.now() < deadline);

    await new Promise(resolve => setTimeout(resolve, 40));
}

describe('RollingFileWriter (§4.3, §5.4)', () => {
    it('does no synchronous fs work on the happy path, then drains async', async () => {
        const file = join(dir, 'nested', 'error.err');
        const writer = new RollingFileWriter({ filePath: file });

        for (let i = 0; i < 100; i++) {
            writer.write(`line ${i}`);
        }

        expect(realFs.appendFileSync).not.toHaveBeenCalled();
        expect(realFs.mkdirSync).not.toHaveBeenCalled();
        expect(realFs.statSync).not.toHaveBeenCalled();
        await waitForDrain(file);
        expect(realFs.appendFileSync).not.toHaveBeenCalled();
        expect(readFileSync(file, 'utf8').split('\n')).toHaveLength(101);
    });

    it('creates the file 0600 inside a 0700 directory', async () => {
        const file = join(dir, 'd', 'error.err');
        const writer = new RollingFileWriter({ filePath: file });
        writer.write('x');
        await waitForDrain(file);
        expect(statSync(file).mode & 0o777).toBe(0o600);
        expect(statSync(join(dir, 'd')).mode & 0o777).toBe(0o700);
    });

    it('a burst over 256 KiB drains synchronously', () => {
        const file = join(dir, 'error.err');
        const writer = new RollingFileWriter({ filePath: file, maxBytes: 10 * 1024 * 1024 });
        const line = 'y'.repeat(1023);

        for (let i = 0; i < 300; i++) {
            writer.write(line);
        }

        expect(realFs.appendFileSync).toHaveBeenCalled();
        expect(statSync(file).size).toBeGreaterThanOrEqual(256 * 1024);
    });

    it('rotates at the cap and keeps at most N backups', () => {
        const file = join(dir, 'error.err');
        const writer = new RollingFileWriter({ filePath: file, maxBytes: 2048, maxBackups: 2 });

        for (let round = 0; round < 6; round++) {
            for (let i = 0; i < 20; i++) {
                writer.write('z'.repeat(99));
            }

            writer.flush();
        }

        expect(statSync(file).size).toBeLessThanOrEqual(2048);
        expect(realFs.existsSync(`${file}.1`)).toBe(true);
        expect(realFs.existsSync(`${file}.2`)).toBe(true);
        expect(realFs.existsSync(`${file}.3`)).toBe(false);
    });

    it('does not roll a file another process already rolled (§4.3)', () => {
        const file = join(dir, 'error.err');
        const writer = new RollingFileWriter({ filePath: file, maxBytes: 2048 });
        writer.write('a'.repeat(1500));
        writer.flush();
        // Another process rolled it: the file is now small.
        writeFileSync(file, 'other\n');
        writer.write('b'.repeat(1000));
        writer.flush();
        expect(realFs.existsSync(`${file}.1`)).toBe(false);
        expect(readFileSync(file, 'utf8').startsWith('other\n')).toBe(true);
    });

    it('two writers appending to one file never interleave mid-line, and only one rolls', () => {
        const file = join(dir, 'error.err');
        const a = new RollingFileWriter({ filePath: file, maxBytes: 4096, maxBackups: 2 });
        const b = new RollingFileWriter({ filePath: file, maxBytes: 4096, maxBackups: 2 });

        for (let i = 0; i < 40; i++) {
            a.write(`A${'a'.repeat(120)}`);
            b.write(`B${'b'.repeat(120)}`);
            a.flush();
            b.flush();
        }

        const all = [file, `${file}.1`, `${file}.2`].filter(f => realFs.existsSync(f)).map(f => readFileSync(f, 'utf8')).join('');
        const lines = all.split('\n').filter(Boolean);
        expect(lines.every(l => /^(Aa{120}|Bb{120})$/.test(l))).toBe(true);
        expect(lines.length).toBeGreaterThan(0);
        expect(realFs.existsSync(`${file}.3`)).toBe(false);
    });

    it('a failing path falls back and does not throw', async () => {
        const blocker = join(dir, 'not-a-dir');
        writeFileSync(blocker, 'file');
        const fallen: string[] = [];
        const writer = new RollingFileWriter({ filePath: join(blocker, 'error.err'), fallback: data => fallen.push(data) });
        expect(() => writer.write('lost?')).not.toThrow();
        await waitForDrain();
        expect(() => writer.flush()).not.toThrow();
        expect(fallen.join('')).toContain('lost?');
    });
});

describe('node sink (M7)', () => {
    it('1,000 caught() calls make no synchronous fs call, and land on disk', async () => {
        const file = join(dir, 'error.err');
        const stdout = vi.spyOn(process.stdout, 'write').mockImplementation(() => true);
        installNodeErrorFile({ app: 'mcp', file, handleProcessErrors: false, echo: false });
        const errors = errorFileFor('mcp/src/errfile/rolling-file-writer.test.ts');

        for (let i = 0; i < 1000; i++) {
            errors.caught(`doing thing ${i % 50}`, new Error('boom'));
        }

        expect(realFs.appendFileSync).not.toHaveBeenCalled();
        expect(realFs.mkdirSync).not.toHaveBeenCalled();
        expect(realFs.statSync).not.toHaveBeenCalled();
        await waitForDrain(file);
        expect(readFileSync(file, 'utf8')).toContain('[ERROR] [mcp] [mcp/src/errfile/rolling-file-writer.test.ts] doing thing 0 — Error: boom');
        expect(stdout).not.toHaveBeenCalled();
        stdout.mockRestore();
    });
});
