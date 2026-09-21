/**
 * T6 — filesystem escape (pm/mcp.mdx §7.0, §9.8, §17 "no-fs-write canary"): no fs.write*, writeFile,
 * appendFile, mkdir, rm, rename outside logger.ts and the credential minter. Staging writes happen
 * in the plane, not here; the vendored error-file library keeps its own file (pm/error_err.mdx).
 */
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import { grep, ownSourceFiles } from './_shared.js';

const FS_WRITE = /fs\.(write|writeSync|writeFile|writeFileSync|appendFile|appendFileSync|mkdir|mkdirSync|rm|rmSync|rmdir|rmdirSync|rename|renameSync|unlink|unlinkSync|open|openSync|chmod|chmodSync|symlink|symlinkSync|createWriteStream|truncate|truncateSync|copyFile|copyFileSync)\(/;
const ALLOWED = new Set(['logger.ts', 'credentials.ts']);

describe('canary: no-fs-write', () => {
  it('confines filesystem writes to logger.ts and credentials.ts', () => {
    const offenders = ownSourceFiles()
      .filter(f => !ALLOWED.has(path.basename(f)))
      .flatMap(f => grep(f, FS_WRITE));
    expect(offenders).toEqual([]);
  });

  it('never imports node:fs outside logger.ts, credentials.ts and prompt.ts (which only reads)', () => {
    const offenders = ownSourceFiles()
      .filter(f => !ALLOWED.has(path.basename(f)) && path.basename(f) !== 'prompt.ts')
      .flatMap(f => grep(f, /from ['"]node:fs['"]|require\(['"]node:fs['"]\)|from ['"]fs['"]/));
    expect(offenders).toEqual([]);
  });

  it('has no tool that takes a path it would write to (staging is the plane\'s)', () => {
    const offenders = ownSourceFiles()
      .filter(f => f.includes(`${path.sep}tools${path.sep}`))
      .flatMap(f => grep(f, /node:fs|writeFile|appendFile|mkdir/));
    expect(offenders).toEqual([]);
  });
});
