/**
 * T9 — no host literal but loopback in the bundle; exactly one module opens a socket
 * (pm/mcp.mdx §3.6, §7.0, §17 "no-network canary", AC 6).
 */
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import { distFiles, grep, ownSourceFiles, read } from './_shared.js';

const HOST_LITERAL = /https?:\/\/(?!127\.0\.0\.1|localhost)[A-Za-z0-9.-]+/;
const IP_LITERAL = /\b(?!127\.)\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b/;
const SOCKET_OPENER = /from ['"]node:(http|https|net|tls|dgram)['"]|require\(['"]node:(http|https|net|tls|dgram)['"]\)|\bfetch\(|undici|new WebSocket\(/;

describe('canary: no-network', () => {
  it('has no host literal other than 127.0.0.1 / localhost in dist/ (outside the instructions prose)', () => {
    const hits = distFiles()
      .filter(f => !f.endsWith('instructions.js'))
      .flatMap(f => grep(f, HOST_LITERAL).filter(h => !/example\.(com|invalid)|books\.example|w3\.org|json-schema\.org/.test(h)));
    expect(hits).toEqual([]);
  });

  it('has no non-loopback IP literal in dist/', () => {
    const hits = distFiles().flatMap(f => grep(f, IP_LITERAL));
    expect(hits).toEqual([]);
  });

  it('opens sockets from src/client.ts only', () => {
    const offenders = ownSourceFiles()
      .filter(f => path.basename(f) !== 'client.ts')
      .flatMap(f => grep(f, SOCKET_OPENER));
    expect(offenders).toEqual([]);
    expect(read(path.join(path.dirname(ownSourceFiles()[0] ?? ''), 'client.ts'))).toMatch(/from 'node:http'/);
  });

  it('does not use fetch (whose undici stamps Sec-Fetch-Mode, which the plane refuses)', () => {
    const hits = ownSourceFiles().flatMap(f => grep(f, /\bfetch\(/));
    expect(hits).toEqual([]);
  });

  it('never names another product\'s credentials or the Actual Budget plane', () => {
    const hits = ownSourceFiles().flatMap(f => grep(f, /actual_budget\.json|\.act3\/credentials|127\.0\.0\.1:5006|EBKTOOL_TOKEN/));
    expect(hits).toEqual([]);
  });
});
