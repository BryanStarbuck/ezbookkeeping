// §15.2 — a fault survives the crash that caused it (R9, M11). A child process installs the node
// sink, reports 3 errors, then dies; the parent reads the file it left behind.
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

const src = pathToFileURL(__dirname).href;

// The child runs the TypeScript source directly: Node strips the types. The library's own imports
// use `.js` specifiers (NodeNext), so a small resolve hook maps `./x.js` to `./x.ts` when only the
// source exists — the same thing tsc's output makes unnecessary once mcp/dist is built.
const HOOKS = `
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
export async function resolve(specifier, context, next) {
    if (specifier.startsWith('.') && specifier.endsWith('.js') && context.parentURL) {
        const ts = new URL(specifier.slice(0, -3) + '.ts', context.parentURL);
        if (existsSync(fileURLToPath(ts))) return next(ts.href, context);
    }
    return next(specifier, context);
}
`;
const REGISTER = `
import { register } from 'node:module';
register('./hooks.mjs', import.meta.url);
`;

function childScript(body: string): string {
    return `
const { installNodeErrorFile } = await import('${src}/node.ts');
const { errorFileFor } = await import('${src}/core.ts');
installNodeErrorFile({ app: 'mcp', file: process.env.CRASH_FILE, echo: false });
const errors = errorFileFor('mcp/src/errfile/crash.test.ts');
errors.caught('reporting the first fault', new Error('one'));
errors.caught('reporting the second fault', new Error('two'));
errors.warn('reporting the third fault', new Error('three'));
${body}
`;
}

let dir = '';

beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'ezbk-errfile-crash-'));
    writeFileSync(join(dir, 'hooks.mjs'), HOOKS);
    writeFileSync(join(dir, 'register.mjs'), REGISTER);
});

afterEach(() => rmSync(dir, { recursive: true, force: true }));

function run(body: string): { status: number | null; text: string; stdout: string; stderr: string } {
    const script = join(dir, 'child.mjs');
    const file = join(dir, 'error.err');
    writeFileSync(script, childScript(body));
    const result = spawnSync(process.execPath, ['--no-warnings', '--import', join(dir, 'register.mjs'), script], {
        env: { ...process.env, CRASH_FILE: file, NODE_ENV: 'production', VITEST: '' },
        encoding: 'utf8',
        timeout: 20_000
    });
    let text = '';

    try {
        text = readFileSync(file, 'utf8');
    } catch {
        text = `(no file) stderr: ${result.stderr}`;
    }

    return { status: result.status, text, stdout: result.stdout, stderr: result.stderr };
}

describe('crash test (§15.2)', () => {
    it('an uncaught exception: 3 records + FATAL on disk, exit(1), nothing on stdout', () => {
        const { status, text, stdout, stderr } = run(`throw new Error('synchronous crash');`);
        expect(status).toBe(1);
        expect(text).toContain('[ERROR] [mcp] [mcp/src/errfile/crash.test.ts] reporting the first fault — Error: one');
        expect(text).toContain('reporting the second fault — Error: two');
        expect(text).toContain('[WARN] [mcp] [mcp/src/errfile/crash.test.ts] reporting the third fault — Error: three');
        expect(text).toMatch(/\[FATAL\] \[mcp\] \[mcp\/src\/index\.ts\] an uncaught exception — Error: synchronous crash/);
        expect(stdout).toBe('');
        expect(stderr).toContain('synchronous crash');
    });

    it('an unhandled rejection: 3 records + the rejection on disk, and the process goes on', () => {
        const { status, text, stdout } = run(`Promise.reject(new Error('rejected crash')); setTimeout(() => process.exit(0), 50);`);
        expect(status).toBe(0);
        expect(text).toContain('reporting the first fault — Error: one');
        expect(text).toMatch(/\[ERROR\] \[mcp\] \[mcp\/src\/index\.ts\] an unhandled promise rejection — Error: rejected crash/);
        expect(text.match(/rejected crash/g)).toHaveLength(1);
        expect(stdout).toBe('');
    });

    it('a normal exit flushes what was buffered', () => {
        const { status, text } = run(`process.exitCode = 0;`);
        expect(status).toBe(0);
        expect(text.split('\n').filter(l => l.startsWith('[')).length).toBe(3);
    });
});
