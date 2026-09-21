/**
 * The transfer tools and the one sanctioned delete — pm/mcp.mdx §9.5b. Scripted plane only; every
 * id and name is synthetic.
 */
import { describe, expect, it } from 'vitest';

import { findTool } from '../src/tools/registry.js';
import { verbTier } from '../src/tools/tool.js';

import { callTool, errorOf, makeHost } from './helpers/fake.js';

describe('ezb_find_transfer_pairs', () => {
  it('reads with the write tier off, posting its arguments to the candidates route', async () => {
    const { host, plane } = makeHost();
    const { ok } = await callTool(host, 'ezb_find_transfer_pairs', { start: '2026-03-01', end: '2026-03-31', window_days: 5, require_hint: false, account_names: ['Northbank Checking ••4021'] });
    expect(ok).toBe(true);
    expect(plane.last().route).toBe('/transactions/transfer-candidates');
    expect(plane.last().opts.method).toBe('POST');
    expect(plane.last().opts.body).toEqual({ start: '2026-03-01', end: '2026-03-31', window_days: 5, require_hint: false, account_names: ['Northbank Checking ••4021'] });
  });

  it('refuses a window over 31 days and unknown keys before any request', async () => {
    const { host, plane } = makeHost();
    expect(errorOf((await callTool(host, 'ezb_find_transfer_pairs', { window_days: 32 })).envelope).code).toBe('invalid_input');
    expect(errorOf((await callTool(host, 'ezb_find_transfer_pairs', { dry_run: true })).envelope).code).toBe('invalid_input');
    expect(plane.calls.length).toBe(0);
  });

  it('is a read tool by its verb', () => {
    expect(findTool('ezb_find_transfer_pairs')?.tier).toBe('read');
    expect(verbTier('ezb_find_transfer_pairs')).toBe('read');
  });
});

describe('ezb_convert_to_transfer', () => {
  it('is a write even though convert_ alone reads: the longest verb wins', () => {
    expect(verbTier('ezb_convert_to_transfer')).toBe('write');
    expect(verbTier('ezb_convert_amount')).toBe('read');
    expect(findTool('ezb_convert_to_transfer')?.tier).toBe('write');
  });

  it('is refused with the write tier off, before any request', async () => {
    const { host, plane } = makeHost();
    const { envelope } = await callTool(host, 'ezb_convert_to_transfer', { items: [{ id: '101', counter_id: '201' }] });
    expect(errorOf(envelope).code).toBe('write_disabled');
    expect(plane.calls.length).toBe(0);
  });

  it('previews by default, sending the items, the options and the ceiling', async () => {
    const { host, plane, config } = makeHost({ env: { EZBKMCP_ALLOW_WRITE: '1' } });
    const items = [{ id: '101', counter_id: '201' }, { id: '102', counter_account_name: 'Market Value' }];
    const { ok } = await callTool(host, 'ezb_convert_to_transfer', { items, comment_mode: 'out', transfer_category_name: 'Moves > Internal' });
    expect(ok).toBe(true);
    expect(plane.last().route).toBe('/transactions/convert-to-transfer');
    expect(plane.last().opts.method).toBe('POST');
    expect(plane.last().opts.body).toEqual({ items, comment_mode: 'out', transfer_category_name: 'Moves > Internal', dry_run: true, max_changes: config.maxChanges });
  });

  it('applies with the echoed token and a raised ceiling', async () => {
    const { host, plane } = makeHost({ env: { EZBKMCP_ALLOW_WRITE: '1' } });
    await callTool(host, 'ezb_convert_to_transfer', { items: [{ id: '101', counter_id: '201' }], dry_run: false, confirm: 'cf_abc', max_changes: 400 });
    expect(plane.last().opts.body).toEqual({ items: [{ id: '101', counter_id: '201' }], dry_run: false, confirm_token: 'cf_abc', max_changes: 400 });
  });

  it('refuses an empty item list, an unknown item key and a UUID id before any request', async () => {
    const { host, plane } = makeHost({ env: { EZBKMCP_ALLOW_WRITE: '1' } });
    expect(errorOf((await callTool(host, 'ezb_convert_to_transfer', { items: [] })).envelope).code).toBe('invalid_input');
    expect(errorOf((await callTool(host, 'ezb_convert_to_transfer', { items: [{ id: '1', counter: '2' }] })).envelope).code).toBe('invalid_input');
    expect(errorOf((await callTool(host, 'ezb_convert_to_transfer', { items: [{ id: '1', counter_id: '2' }], comment_mode: 'neither' })).envelope).code).toBe('invalid_input');
    const uuid = await callTool(host, 'ezb_convert_to_transfer', { items: [{ id: '1d6f0a4e-8b2c-4c1a-9d3e-2f5b7a9c0e11', counter_id: '2' }] });
    expect(['wrong_server', 'invalid_input']).toContain(errorOf(uuid.envelope).code);
    expect(plane.calls.length).toBe(0);
  });
});

describe('ezb_delete_transactions (the one admin tool)', () => {
  it('needs the admin switch on top of the write switch, and is listed as disabled without it', async () => {
    const { host, plane } = makeHost({ env: { EZBKMCP_ALLOW_WRITE: '1' } });
    const listed = host.handleListTools().tools.find(t => t.name === 'ezb_delete_transactions');
    expect(listed?.description).toContain('CURRENTLY DISABLED: the admin tier is off');
    expect(listed?.description).toContain('EZBKMCP_ALLOW_ADMIN=1');
    const { envelope } = await callTool(host, 'ezb_delete_transactions', { ids: ['101'] });
    expect(errorOf(envelope).code).toBe('write_disabled');
    expect(plane.calls.length).toBe(0);
  });

  it('previews by default through DELETE /transactions/bulk with ids only', async () => {
    const { host, plane, config } = makeHost({ env: { EZBKMCP_ALLOW_WRITE: '1', EZBKMCP_ALLOW_ADMIN: '1' } });
    const { ok } = await callTool(host, 'ezb_delete_transactions', { ids: ['101', '102'] });
    expect(ok).toBe(true);
    expect(plane.last().route).toBe('/transactions/bulk');
    expect(plane.last().opts.method).toBe('DELETE');
    expect(plane.last().opts.body).toEqual({ ids: ['101', '102'], dry_run: true, max_changes: config.maxChanges });
  });

  it('refuses a filter and an empty id list', async () => {
    const { host, plane } = makeHost({ env: { EZBKMCP_ALLOW_WRITE: '1', EZBKMCP_ALLOW_ADMIN: '1' } });
    expect(errorOf((await callTool(host, 'ezb_delete_transactions', { ids: [] })).envelope).code).toBe('invalid_input');
    expect(errorOf((await callTool(host, 'ezb_delete_transactions', { ids: ['1'], filter: { start: '2026-01-01' } })).envelope).code).toBe('invalid_input');
    expect(plane.calls.length).toBe(0);
  });
});
