/**
 * The shared credentials vectors — pm/mcp.mdx §4 "Shared rules with the CLI, not shared code",
 * §6.3, §7.7, §17 "Auth negatives". pkg/machine/testdata/credentials_vectors.json is the committed
 * behaviour all three implementations (Go server, Go CLI, this file) must agree on.
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { CredentialsError, checkCredentialsFile, fingerprint, isWellFormedKey, mintKey, readProduct, resolveKey } from '../src/credentials.js';

import { tempDir } from './helpers/fake.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const VECTORS_PATH = path.resolve(here, '..', '..', 'pkg', 'machine', 'testdata', 'credentials_vectors.json');

type Vectors = {
  wellFormed: Array<{ key: string; fingerprint: string }>;
  malformed: Array<{ key: string; why: string }>;
  credentialsFile: Record<string, unknown>;
};

const vectors = JSON.parse(fs.readFileSync(VECTORS_PATH, 'utf8')) as Vectors;

function tmp(): string {
  return tempDir('ezbkmcp-creds-');
}

describe('credentials vectors (pkg/machine/testdata/credentials_vectors.json)', () => {
  it('has vectors', () => {
    expect(vectors.wellFormed.length).toBeGreaterThan(0);
    expect(vectors.malformed.length).toBeGreaterThan(0);
  });

  it('accepts every well-formed key and fingerprints it exactly as the Go copies do', () => {
    for (const v of vectors.wellFormed) {
      expect(isWellFormedKey(v.key), v.key).toBe(true);
      expect(fingerprint(v.key)).toBe(v.fingerprint);
      expect(fingerprint(v.key)).not.toContain(v.key.slice(4, 12));
    }
    expect(fingerprint('')).toBe('none');
  });

  it('refuses every malformed key, never padding, trimming or lowercasing', () => {
    for (const v of vectors.malformed) {
      expect(isWellFormedKey(v.key), `${JSON.stringify(v.key)} (${v.why}) must be refused`).toBe(false);
    }
  });

  it('mints 64 lowercase hex from a CSPRNG, unique each time', () => {
    const seen = new Set<string>();
    for (let i = 0; i < 64; i++) {
      const k = mintKey();
      expect(k).toMatch(/^[0-9a-f]{64}$/);
      expect(seen.has(k)).toBe(false);
      seen.add(k);
    }
  });

  it('merges into the vectors\' credentials file, preserving every other product\'s subtree byte for byte', () => {
    const dir = tmp();
    const file = path.join(dir, 'ezbookkeeping.json');
    fs.writeFileSync(file, JSON.stringify(vectors.credentialsFile, null, 2), { mode: 0o600 });

    // No key yet in the vectors file: resolve without minting says so.
    expect(resolveKey({ env: {}, file, mint: false })).toBeNull();

    const minted = resolveKey({ env: {}, file, mint: true, mintedBy: 'mcp' });
    expect(minted?.source).toBe('minted');
    expect(minted?.key).toMatch(/^[0-9a-f]{64}$/);

    const after = JSON.parse(fs.readFileSync(file, 'utf8')) as Record<string, unknown>;
    expect(after.actual_budget).toEqual(vectors.credentialsFile.actual_budget);
    expect(after.large_files_bridge).toEqual(vectors.credentialsFile.large_files_bridge);
    const product = after.ezbookkeeping as { machine: Record<string, unknown>; statements: unknown; timezone: unknown };
    expect(product.statements).toEqual((vectors.credentialsFile.ezbookkeeping as { statements: unknown }).statements);
    expect(product.timezone).toEqual((vectors.credentialsFile.ezbookkeeping as { timezone: unknown }).timezone);
    expect(product.machine.username).toBe('operator');
    expect(product.machine.label).toBe('synthetic-host');
    expect(product.machine.api_key).toBe(minted?.key);
    expect(product.machine.created_by).toBe('mcp');
    expect((fs.statSync(file).mode & 0o777).toString(8)).toBe('600');

    // A second resolve reuses the key unchanged (compare-and-set).
    const again = resolveKey({ env: {}, file, mint: true, mintedBy: 'mcp' });
    expect(again?.key).toBe(minted?.key);
    expect(again?.source).toBe('credentials-file');
    expect(readProduct(file).timezone).toBe('America/Los_Angeles');
  });

  it('refuses a 0644 file with the exact chmod (§6.3)', () => {
    const dir = tmp();
    const file = path.join(dir, 'ezbookkeeping.json');
    fs.writeFileSync(file, '{}', { mode: 0o644 });
    fs.chmodSync(file, 0o644);
    expect(() => checkCredentialsFile(file)).toThrow(CredentialsError);
    try {
      checkCredentialsFile(file);
    } catch (err) {
      expect((err as CredentialsError).fix).toBe(`chmod 600 ${file}`);
      expect((err as CredentialsError).message).toContain('644');
    }
  });

  it('refuses a symlink at read and at write', () => {
    const dir = tmp();
    const target = path.join(dir, 'target.json');
    fs.writeFileSync(target, '{}', { mode: 0o600 });
    const link = path.join(dir, 'ezbookkeeping.json');
    fs.symlinkSync(target, link);
    expect(() => checkCredentialsFile(link)).toThrow(/symlink/);
    expect(() => resolveKey({ env: {}, file: link, mint: true })).toThrow(/symlink/);
    expect(fs.readFileSync(target, 'utf8')).toBe('{}');
  });

  it('refuses a malformed key on disk with the rotate hint, and honours the env overrides in order', () => {
    const dir = tmp();
    const file = path.join(dir, 'ezbookkeeping.json');
    fs.writeFileSync(file, JSON.stringify({ ezbookkeeping: { machine: { api_key: 'not-a-key' } } }), { mode: 0o600 });
    expect(() => resolveKey({ env: {}, file, mint: false })).toThrow(/rotate/);

    const good = vectors.wellFormed[0]?.key ?? '';
    const other = vectors.wellFormed[1]?.key ?? '';
    const keyFile = path.join(dir, 'key.txt');
    fs.writeFileSync(keyFile, `${other}\n`);
    expect(resolveKey({ env: { EZBKMCP_API_KEY: good, EZBKMCP_API_KEY_FILE: keyFile }, file, mint: false })).toMatchObject({ key: good, source: 'EZBKMCP_API_KEY' });
    expect(resolveKey({ env: { EZBKMCP_API_KEY_FILE: keyFile }, file, mint: false })).toMatchObject({ key: other, source: 'EZBKMCP_API_KEY_FILE' });
    expect(resolveKey({ env: { EZBK_API_KEY: good }, file, mint: false })).toMatchObject({ key: good, source: 'EZBK_API_KEY' });
    expect(() => resolveKey({ env: { EZBKMCP_API_KEY: 'BAD' }, file, mint: false })).toThrow(CredentialsError);
  });

  it('never uses Math.random anywhere under src/', () => {
    const src = path.resolve(here, '..', 'src');
    const files: string[] = [];
    const walk = (d: string): void => {
      for (const e of fs.readdirSync(d, { withFileTypes: true })) {
        const p = path.join(d, e.name);
        if (e.isDirectory()) {
          walk(p);
        } else if (p.endsWith('.ts')) {
          files.push(p);
        }
      }
    };
    walk(src);
    for (const f of files) {
      expect(fs.readFileSync(f, 'utf8'), f).not.toContain('Math.random');
    }
  });
});
