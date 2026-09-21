/**
 * T16 — the upstream full-access door (pm/mcp.mdx §7.0, §17 "upstream-door canary"): ezb_health
 * reports upstream's enable_api_token / enable_mcp, and the report is true when a fixture server
 * has them on. This server never uses either door.
 *
 * The in-process half runs always; the live half needs the Go binary and skips with a reason.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { StdioRpcClient } from '../../test/helpers/rpc.js';
import { startLiveServer } from '../../test/helpers/live.js';
import type { LiveServer } from '../../test/helpers/live.js';

import { callTool, FakePlane, makeHost } from '../../test/helpers/fake.js';

import { ownSourceFiles, grep } from './_shared.js';

let live: LiveServer | null = null;
let reason = 'no ezBookkeeping binary with the machine plane (set EZBK_SERVER_BIN)';

describe('canary: upstream-door', () => {
  beforeAll(async () => {
    try {
      // Upstream's two doors switched ON through its own env overrides, in a throwaway install.
      live = await startLiveServer({ allowWrite: false, registerUser: true, serverEnv: { EBK_SECURITY_ENABLE_API_TOKEN: 'true', EBK_MCP_ENABLE_MCP: 'true' } });
    } catch (err) {
      reason = (err as Error).message;
      live = null;
    }
  }, 90_000);

  afterAll(async () => {
    await live?.stop();
  });

  it('ezb_health passes the plane\'s upstreamDoors report through verbatim', async () => {
    const plane = new FakePlane();
    plane.script = () => ({ ok: true, data: { healthy: true, upstreamDoors: { apiToken: true, mcp: true } }, meta: { user: 'operator' } });
    const { host } = makeHost({ plane });
    const { envelope } = await callTool(host, 'ezb_health', {});
    expect((envelope.data as { upstreamDoors: { apiToken: boolean; mcp: boolean } }).upstreamDoors).toEqual({ apiToken: true, mcp: true });
  });

  it('never uses upstream\'s /mcp endpoint, its API tokens or Authorization: Bearer', () => {
    const hits = ownSourceFiles().flatMap(f => grep(f, /\/api\/v1\/|['"]\/mcp['"]|Authorization|Bearer|token_record|EBKTOOL/));
    expect(hits).toEqual([]);
  });

  it('reports both doors ON against a fixture server that has them on (live)', async ({ skip }) => {
    if (live === null) {
      skip(reason);
      return;
    }
    const client = new StdioRpcClient(live.mcpEnv());
    try {
      await client.initialize();
      const { envelope } = await client.call('ezb_health', {});
      expect(envelope.ok).toBe(true);
      const doors = (envelope.data as { upstreamDoors: { apiToken: boolean; mcp: boolean } }).upstreamDoors;
      expect(doors).toEqual({ apiToken: true, mcp: true });
      const caps = await client.call('ezb_capabilities', {});
      expect((caps.envelope.data as { upstreamFeatures: { apiToken: boolean; mcp: boolean } }).upstreamFeatures).toMatchObject({ apiToken: true, mcp: true });
    } finally {
      await client.close();
    }
  }, 60_000);
});
