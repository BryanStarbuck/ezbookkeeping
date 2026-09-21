// §5.5 / §15.1 — the six shared files in mcp/src/errfile match src/lib/errfile byte for byte
// after the GENERATED header. "Copy" never means "drift".
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const root = join(__dirname, '..', '..', '..');
const HEADER = '// GENERATED from src/lib/errfile — run scripts/sync-errfile.sh\n';
const FILES = ['index.ts', 'core.ts', 'describe.ts', 'redact.ts', 'fold.ts', 'format.ts'];

describe('vendor drift', () => {
    for (const file of FILES) {
        it(`mcp/src/errfile/${file} matches src/lib/errfile/${file}`, () => {
            const source = join(root, 'src', 'lib', 'errfile', file);
            const copy = join(__dirname, file);
            expect(existsSync(copy), `${copy} is missing — run scripts/sync-errfile.sh`).toBe(true);
            const text = readFileSync(copy, 'utf8');
            expect(text.startsWith(HEADER), `${copy} lacks the GENERATED header`).toBe(true);
            expect(text.slice(HEADER.length) === readFileSync(source, 'utf8'), `mcp/src/errfile/${file} has drifted — run scripts/sync-errfile.sh`).toBe(true);
        });
    }

    it('the shared files import nothing outside src/lib/errfile', () => {
        for (const file of FILES) {
            const text = readFileSync(join(root, 'src', 'lib', 'errfile', file), 'utf8');
            const specifiers = [...text.matchAll(/^(?:import|export)[^\n]*?\bfrom\s+'([^']+)'/gm)].map(m => m[1]);

            for (const spec of specifiers) {
                expect(spec, `${file} imports ${spec}`).toMatch(/^\.\/(index|core|describe|redact|fold|format)\.js$/);
            }
        }
    });

    it('the sync script, when present, copies exactly these files with this header', () => {
        const script = join(root, 'scripts', 'sync-errfile.sh');

        if (!existsSync(script)) {
            return; // B1 owns scripts/sync-errfile.sh; until it lands, the copies above are checked on their own
        }

        const text = readFileSync(script, 'utf8');

        for (const file of FILES) {
            expect(text, `sync-errfile.sh does not mention ${file}`).toContain(file);
        }

        expect(text).toContain('GENERATED from src/lib/errfile');
    });
});
