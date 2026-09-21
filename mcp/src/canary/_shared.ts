/**
 * Helpers shared by the canaries in this directory (pm/mcp.mdx §7.0, §17). Not a canary itself:
 * the parity test only counts `*.canary.ts` files.
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
export const MCP_ROOT = path.resolve(here, '..', '..');
export const REPO_ROOT = path.resolve(MCP_ROOT, '..');
export const SRC_DIR = path.join(MCP_ROOT, 'src');
export const DIST_DIR = path.join(MCP_ROOT, 'dist');

/** Every file under a directory, recursively, filtered by extension. */
export function filesUnder(dir: string, ext: string, skip: (p: string) => boolean = () => false): string[] {
  const out: string[] = [];
  if (!fs.existsSync(dir)) {
    return out;
  }
  const walk = (d: string): void => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (skip(p)) {
        continue;
      }
      if (e.isDirectory()) {
        walk(p);
      } else if (p.endsWith(ext)) {
        out.push(p);
      }
    }
  };
  walk(dir);
  return out.sort();
}

/** Our own source files: not the vendored error-file library, not tests, not canaries. */
export function ownSourceFiles(): string[] {
  return filesUnder(SRC_DIR, '.ts', p => p.includes(`${path.sep}errfile${path.sep}`) || p.includes(`${path.sep}canary${path.sep}`) || p.endsWith('.test.ts'));
}

/** The built bundle: every .js under dist/ (our code compiled; the SDK stays in node_modules). */
export function distFiles(): string[] {
  return filesUnder(DIST_DIR, '.js');
}

export function read(p: string): string {
  return fs.readFileSync(p, 'utf8');
}

/** Lines of a file that match, with their numbers, for a readable failure. */
export function grep(p: string, re: RegExp): string[] {
  const hits: string[] = [];
  read(p)
    .split('\n')
    .forEach((line, i) => {
      if (re.test(line)) {
        hits.push(`${path.relative(MCP_ROOT, p)}:${String(i + 1)}: ${line.trim()}`);
      }
    });
  return hits;
}

/** Walk a JSON value, calling fn with each object and the path of keys leading to it. */
export function walkJson(value: unknown, fn: (obj: Record<string, unknown>, trail: Array<Record<string, unknown>>, keyPath: string) => void, trail: Array<Record<string, unknown>> = [], keyPath = '$'): void {
  if (Array.isArray(value)) {
    value.forEach((v, i) => walkJson(v, fn, trail, `${keyPath}[${String(i)}]`));
    return;
  }
  if (value !== null && typeof value === 'object') {
    const obj = value as Record<string, unknown>;
    fn(obj, trail, keyPath);
    for (const [k, v] of Object.entries(obj)) {
      walkJson(v, fn, [...trail, obj], `${keyPath}.${k}`);
    }
  }
}
