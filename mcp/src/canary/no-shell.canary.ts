/**
 * T9 — no child_process, no eval, no new Function, no vm (pm/mcp.mdx §7.0, §17 "no-shell canary", AC 6).
 */
import { describe, expect, it } from 'vitest';

import { distFiles, grep, ownSourceFiles } from './_shared.js';

const SHELL = /child_process|\beval\(|new Function\(|from ['"]node:vm['"]|require\(['"]vm['"]\)|require\(['"]node:vm['"]\)/;

describe('canary: no-shell', () => {
  it('has no child_process, eval, new Function or vm in dist/', () => {
    expect(distFiles().flatMap(f => grep(f, SHELL))).toEqual([]);
  });

  it('has none in src/ either', () => {
    expect(ownSourceFiles().flatMap(f => grep(f, SHELL))).toEqual([]);
  });

  it('declares exactly two runtime dependencies', async () => {
    const pkg = (await import('../../package.json', { with: { type: 'json' } })).default as { dependencies: Record<string, string> };
    expect(Object.keys(pkg.dependencies).sort()).toEqual(['@modelcontextprotocol/sdk', 'zod']);
    for (const v of Object.values(pkg.dependencies)) {
      expect(v, 'pinned exact: no ^ or ~').toMatch(/^\d+\.\d+\.\d+$/);
    }
  });
});
