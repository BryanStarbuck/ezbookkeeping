/**
 * An in-process host with a scripted machine plane — for the catalogue, gate, routing, envelope
 * and audit tests that must not need a live server.
 */
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { afterAll } from 'vitest';

import { interpretEnvelope } from '../../src/client.js';
import type { MachinePlaneClient, PlaneResponse, RequestOptions } from '../../src/client.js';
import { loadConfig } from '../../src/config.js';
import type { Config } from '../../src/config.js';
import { Logger } from '../../src/logger.js';
import { McpServerHost } from '../../src/server.js';

export type Recorded = { route: string; opts: RequestOptions };

export class FakePlane {
  readonly calls: Recorded[] = [];
  /** Either a response, or a function computing one from the call. */
  script: (route: string, opts: RequestOptions) => PlaneResponse | Error = () => ({
    ok: true,
    data: { fake: true },
    meta: { user: 'operator', defaultCurrency: 'USD', timezone: 'UTC', asOf: '2026-09-21T00:00:00Z', serverVersion: '2.0.1', truncated: false, target: 'local' },
  });

  request(route: string, opts: RequestOptions = {}): Promise<PlaneResponse> {
    this.calls.push({ route, opts });
    const out = this.script(route, opts);
    if (out instanceof Error) {
      return Promise.reject(out);
    }
    try {
      return Promise.resolve(interpretEnvelope(out, opts.method ?? 'GET', route));
    } catch (err) {
      return Promise.reject(err instanceof Error ? err : new Error(String(err)));
    }
  }

  last(): Recorded {
    const r = this.calls[this.calls.length - 1];
    if (r === undefined) {
      throw new Error('no call was made');
    }
    return r;
  }
}

export function testConfig(env: Record<string, string> = {}): Config {
  return loadConfig({ ...env });
}

const created: string[] = [];

/** A temp directory that is removed when the test process exits. */
export function tempDir(prefix = 'ezbkmcp-test-'): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  created.push(dir);
  return dir;
}

// Registered at import time, so every test file that uses tempDir() cleans up after itself.
afterAll(() => {
  for (const dir of created.splice(0)) {
    try {
      fs.rmSync(dir, { recursive: true, force: true });
    } catch {
      // a leftover temp dir is not a failed test
    }
  }
});

export function makeHost(opts: { env?: Record<string, string>; plane?: FakePlane; logDir?: string } = {}): { host: McpServerHost; plane: FakePlane; config: Config; logDir: string } {
  const logDir = opts.logDir ?? tempDir('ezbkmcp-log-');
  const config = testConfig({ EZBKMCP_LOG_DIR: logDir, ...(opts.env ?? {}) });
  const plane = opts.plane ?? new FakePlane();
  const logger = new Logger({ dir: logDir, level: 'info' });
  const host = new McpServerHost({
    config,
    client: plane as unknown as MachinePlaneClient,
    logger,
    keyFingerprint: '0123…/sha256:a8ae',
    instructions: 'test instructions',
  });
  return { host, plane, config, logDir };
}

export async function callTool(host: McpServerHost, name: string, args: unknown = {}): Promise<{ ok: boolean; envelope: Record<string, unknown>; isError: boolean }> {
  const res = await host.handleCallTool(name, args);
  const text = res.content[0]?.text ?? '{}';
  const envelope = JSON.parse(text) as Record<string, unknown>;
  return { ok: envelope.ok === true, envelope, isError: res.isError === true };
}

export function errorOf(envelope: Record<string, unknown>): { code: string; message: string; hint?: string; details?: unknown } {
  return envelope.error as { code: string; message: string; hint?: string; details?: unknown };
}
