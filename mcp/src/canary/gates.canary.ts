/**
 * T1 / T2 — money moved or destroyed, silent large write (pm/mcp.mdx §7.0): the write tier is off
 * by default with TWO switches, no delete tool exists, every write previews by default, the ceiling
 * refuses with the real count, and ezb_undo exists and ships with the writes.
 */
import { describe, expect, it } from 'vitest';

import { loadConfig } from '../config.js';
import { findTool, TOOLS } from '../tools/registry.js';
import { ADMIN_TOOLS } from '../tools/tool.js';

import { callTool, errorOf, FakePlane, makeHost } from '../../test/helpers/fake.js';

describe('canary: gates', () => {
  it('has the write tier OFF unless EZBKMCP_ALLOW_WRITE=1 exactly', () => {
    expect(loadConfig({}).allowWrite).toBe(false);
    expect(loadConfig({ EZBKMCP_ALLOW_WRITE: 'true' }).allowWrite).toBe(false);
    expect(loadConfig({ EZBKMCP_ALLOW_WRITE: 'yes' }).allowWrite).toBe(false);
    expect(loadConfig({ EZBKMCP_ALLOW_WRITE: '1' }).allowWrite).toBe(true);
    expect(loadConfig({ EZBK_MACHINE_ALLOW_WRITE: '1' }).allowWrite, "the server's switch alone is not this side's").toBe(false);
  });

  it('has no tool that deletes, clears, resets, rotates or mints a token — but the one sanctioned ids-only delete (§9.5b)', () => {
    for (const tool of TOOLS) {
      expect(tool.route.path).not.toMatch(/\/admin\/|\/api\/|\/redo$|\/batch$/);
      if ((ADMIN_TOOLS as readonly string[]).includes(tool.name)) {
        continue;
      }
      expect(tool.name).not.toMatch(/delete|remove_account|remove_transactions?$|remove_categor|remove_tag$|clear|reset|rotate|token|password|session|2fa|oauth|recogni/);
      expect(tool.route.method).not.toBe('DELETE');
    }
    expect(findTool('ezb_delete_transaction')).toBeUndefined();
    // the operator asked for exactly one removal tool: ids only, previewed, confirmed, admin-gated
    const del = findTool('ezb_delete_transactions');
    expect(del?.admin).toBe(true);
    expect(del?.hasDryRun).toBe(true);
    expect(del?.route).toEqual({ method: 'DELETE', path: '/transactions/bulk' });
    expect(Object.keys(del?.inputSchema.properties as Record<string, unknown>)).not.toContain('filter');
    expect(TOOLS.filter(t => t.route.method === 'DELETE').map(t => t.name)).toEqual(['ezb_delete_transactions']);
  });

  it('never takes an admin switch as an argument, and admin needs BOTH this side\'s switches exactly', () => {
    const schemas = JSON.stringify(TOOLS.map(t => t.inputSchema));
    expect(schemas).not.toMatch(/allow_admin|EZBK_MACHINE_ALLOW_ADMIN|tier/);
    expect(loadConfig({}).allowAdmin).toBe(false);
    expect(loadConfig({ EZBKMCP_ALLOW_ADMIN: '1', EZBK_MACHINE_ALLOW_ADMIN: '1' }).allowAdmin, 'admin without the write switch').toBe(false);
    expect(loadConfig({ EZBKMCP_ALLOW_WRITE: '1', EZBK_MACHINE_ALLOW_ADMIN: '1' }).allowAdmin, "the server's switch alone is not this side's").toBe(false);
    expect(loadConfig({ EZBKMCP_ALLOW_WRITE: '1', EZBKMCP_ALLOW_ADMIN: 'true' }).allowAdmin).toBe(false);
    expect(loadConfig({ EZBKMCP_ALLOW_WRITE: '1', EZBKMCP_ALLOW_ADMIN: '1' }).allowAdmin).toBe(true);
  });

  it('refuses the admin tool with the write switch alone, naming the admin switches, before any request', async () => {
    const { host, plane } = makeHost({ env: { EZBKMCP_ALLOW_WRITE: '1' } });
    const { ok, envelope } = await callTool(host, 'ezb_delete_transactions', { ids: ['1'] });
    expect(ok).toBe(false);
    expect(errorOf(envelope).code).toBe('write_disabled');
    expect(errorOf(envelope).hint).toContain('EZBKMCP_ALLOW_ADMIN=1');
    expect(errorOf(envelope).hint).toContain('--allow-admin');
    expect(plane.calls.length).toBe(0);
  });

  it('identifies itself to the plane as ezbookkeeping-mcp, which the plane refuses on admin routes', async () => {
    const { MachinePlaneClient } = await import('../client.js');
    expect(MachinePlaneClient.clientHeader()).toMatch(/^ezbookkeeping-mcp\/\d+\.\d+\.\d+$/);
  });

  it('ezb_undo exists in the write tier, with no dry_run, and reports what it reversed', async () => {
    const undo = findTool('ezb_undo');
    expect(undo?.tier).toBe('write');
    expect(undo?.hasDryRun).toBe(false);
    expect(undo?.route).toEqual({ method: 'POST', path: '/undo' });
    const plane = new FakePlane();
    plane.script = () => ({ ok: true, data: { entry: { route: 'POST /transactions', undone: true, summary: 'created 1 transaction' } }, meta: { user: 'operator', defaultCurrency: 'USD' } });
    const { host } = makeHost({ env: { EZBKMCP_ALLOW_WRITE: '1' }, plane });
    const { ok, envelope } = await callTool(host, 'ezb_undo', {});
    expect(ok).toBe(true);
    expect(JSON.stringify(envelope.data)).toContain('created 1 transaction');
  });

  it('refuses a write over the ceiling with the real count, even as a preview', async () => {
    const plane = new FakePlane();
    plane.script = (_route, opts) => {
      const body = opts.body as { max_changes?: number };
      return { ok: false, error: { code: 'conflict', message: 'x', details: { would_change: 5000, max_changes: body.max_changes ?? 200, changes: { update: 5000 } } } };
    };
    const { host } = makeHost({ env: { EZBKMCP_ALLOW_WRITE: '1', EZBKMCP_MAX_CHANGES: '150' }, plane });
    const { envelope } = await callTool(host, 'ezb_add_transaction_tags', { filter: { start: '2000-01-01' }, tag_ids: ['1'] });
    const err = errorOf(envelope);
    expect(err.code).toBe('too_many_changes');
    expect(err.message).toContain('5000');
    expect(err.message).toContain('150');
    expect((plane.last().opts.body as { max_changes: number }).max_changes).toBe(150);
  });
});
