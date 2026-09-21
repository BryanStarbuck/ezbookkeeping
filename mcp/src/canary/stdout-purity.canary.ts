/**
 * T13 — stdout corruption (pm/mcp.mdx §8.1, §17 "stdout-purity canary", AC 5).
 *
 * Greps the BUILT dist/ for the three stdout writers, and runs a scripted session whose stdout must
 * parse line by line as JSON-RPC.
 */
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import { StdioRpcClient } from '../../test/helpers/rpc.js';

import { DIST_DIR, distFiles, grep } from './_shared.js';

describe('canary: stdout-purity', () => {
  it('has a built dist/ to inspect', () => {
    expect(fs.existsSync(path.join(DIST_DIR, 'index.js')), 'run `npm run build` first').toBe(true);
  });

  it('holds no console.log(, console.info( or process.stdout.write( in dist/', () => {
    const hits = distFiles().flatMap(f => grep(f, /console\.log\(|console\.info\(|process\.stdout\.write\(/));
    expect(hits).toEqual([]);
  });

  it('holds no console.log( or process.stdout.write( in the vendored error-file library either', () => {
    const hits = distFiles()
      .filter(f => f.includes(`${path.sep}errfile${path.sep}`))
      .flatMap(f => grep(f, /console\.log\(|process\.stdout\.write\(/));
    expect(hits).toEqual([]);
  });

  it('answers a scripted session with nothing but JSON-RPC on stdout, even with the app down', async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ezbkmcp-purity-'));
    const client = new StdioRpcClient({
      HOME: dir,
      EZBKMCP_API_URL: 'http://127.0.0.1:1', // nothing listens here: every tool answers not_ready
      EZBKMCP_CREDENTIALS_FILE: path.join(dir, 'creds.json'),
      EZBKMCP_LOG_DIR: path.join(dir, 'log'),
      EZBK_ERROR_FILE: path.join(dir, 'error.err'),
      EZBKMCP_TIMEOUT_MS: '2000',
    });
    try {
      const init = await client.initialize();
      expect(init.error).toBeUndefined();
      const result = init.result as { serverInfo: { name: string }; capabilities: Record<string, unknown>; instructions: string };
      expect(result.serverInfo.name).toBe('ezbookkeeping');
      expect(result.capabilities).toEqual({ tools: {} });
      expect(result.instructions.startsWith("Is this about the operator's own money kept in **ezBookkeeping** on THIS computer")).toBe(true);

      const tools = await client.listTools();
      expect(tools.length).toBe(65);

      const health = await client.call('ezb_health', {});
      expect(health.raw.isError).toBe(true);
      expect((health.envelope.error as { code: string; hint: string }).code).toBe('not_ready');
      expect((health.envelope.error as { code: string; hint: string }).hint).toContain('ezbk up');

      // prompts/* and resources/* answer -32601 (§8, invariant 4)
      const prompts = await client.request('prompts/list');
      expect(prompts.error?.code).toBe(-32601);
      const resources = await client.request('resources/list');
      expect(resources.error?.code).toBe(-32601);

      for (const line of client.rawStdoutLines) {
        const parsed = JSON.parse(line) as { jsonrpc?: string };
        expect(parsed.jsonrpc).toBe('2.0');
      }
      expect(client.rawStdoutLines.length).toBeGreaterThanOrEqual(5);
      // The banner went to stderr, with the key only as a fingerprint.
      const stderr = client.stderr.join('');
      expect(stderr).toContain('ezbookkeeping 0.1.0 -> http://127.0.0.1:1');
      expect(stderr).not.toMatch(/[0-9a-f]{64}/);
    } finally {
      await client.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  }, 30_000);
});
