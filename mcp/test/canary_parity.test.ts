/**
 * Canary/threat parity — pm/mcp.mdx §4, §17, AC 19: every row of §7.0's threat table names a canary,
 * and every canary named there is a file in src/canary/ — both directions.
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

const here = path.dirname(fileURLToPath(import.meta.url));
const SPEC = path.resolve(here, '..', '..', 'pm', 'mcp.mdx');
const CANARY_DIR = path.resolve(here, '..', 'src', 'canary');

function canaryColumn(): string[] {
  const text = fs.readFileSync(SPEC, 'utf8');
  const start = text.indexOf('### 7.0 Threat model at a glance');
  const end = text.indexOf('### 7.1');
  expect(start, 'pm/mcp.mdx §7.0 not found').toBeGreaterThan(0);
  const table = text.slice(start, end);
  const names = new Set<string>();
  for (const line of table.split('\n')) {
    if (!/^\| T\d+/.test(line)) {
      continue;
    }
    const cells = line.split('|').map(c => c.trim());
    const last = cells[cells.length - 2] ?? '';
    for (const m of last.matchAll(/`([a-z-]+)`/g)) {
      names.add(m[1] ?? '');
    }
  }
  return [...names].sort();
}

describe('canary/threat parity (§7.0 ↔ src/canary/)', () => {
  it('every canary named in §7.0 is a *.canary.ts file, and every *.canary.ts file is named in §7.0', () => {
    const named = canaryColumn();
    expect(named.length).toBeGreaterThan(5);
    const files = fs
      .readdirSync(CANARY_DIR)
      .filter(f => f.endsWith('.canary.ts'))
      .map(f => f.replace(/\.canary\.ts$/, ''))
      .sort();
    expect(files).toEqual(named);
  });

  it('every threat row has a canary or an explicit dash', () => {
    const text = fs.readFileSync(SPEC, 'utf8');
    const table = text.slice(text.indexOf('### 7.0'), text.indexOf('### 7.1'));
    for (const line of table.split('\n').filter(l => /^\| T\d+/.test(l))) {
      const cells = line.split('|').map(c => c.trim());
      const last = cells[cells.length - 2] ?? '';
      expect(last === '—' || /`[a-z-]+`/.test(last), line).toBe(true);
    }
  });
});
