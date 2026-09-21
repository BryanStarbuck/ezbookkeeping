/**
 * T3 — ledger read as another's (pm/mcp.mdx §3, §7.0): the seven layers, checked where they are
 * checkable in-process — the server key, the prefix, the fourth clause, the instructions' first
 * sentence naming Actual Budget, the refuse-with-redirect, and `app` on every envelope.
 */
import { describe, expect, it } from 'vitest';

import { INSTRUCTIONS } from '../instructions.js';
import { SERVER_NAME } from '../server.js';
import { TOOLS } from '../tools/registry.js';
import { WHICH_SERVER } from '../tools/tool.js';

import { callTool, errorOf, makeHost } from '../../test/helpers/fake.js';

describe('canary: routing', () => {
  it('layer 1–3: server key ezbookkeeping, and ezb_ on every tool', () => {
    expect(SERVER_NAME).toBe('ezbookkeeping');
    for (const t of TOOLS) {
      expect(t.name.startsWith('ezb_')).toBe(true);
      expect(t.name.startsWith('ab_')).toBe(false);
    }
  });

  it('layer 4: every description ends with the clause naming the app and its three neighbours', () => {
    for (const t of TOOLS) {
      expect(t.description.endsWith(WHICH_SERVER)).toBe(true);
    }
    expect(WHICH_SERVER).toContain('ezBookkeeping');
    expect(WHICH_SERVER).toContain('`actual_budget`');
    expect(WHICH_SERVER).toContain('`quickbooks`');
    expect(WHICH_SERVER).toContain('`act3`');
  });

  it('layer 5: the instructions open with routing, naming Actual Budget, and say to ask once', () => {
    const firstSentence = INSTRUCTIONS.split('. ')[0] ?? '';
    expect(firstSentence).toContain('ezBookkeeping');
    expect(INSTRUCTIONS.slice(0, 600)).toContain('Actual Budget');
    expect(INSTRUCTIONS).toMatch(/ask once/i);
    expect(INSTRUCTIONS).toMatch(/never add the two/i);
  });

  it('layer 6: a foreign-shaped argument is refused with a redirect naming the right server', async () => {
    const { host, plane } = makeHost();
    const uuid = errorOf((await callTool(host, 'ezb_get_account', { account_id: '11111111-2222-4333-8444-555555555555' })).envelope);
    expect(uuid.code).toBe('wrong_server');
    expect(uuid.hint).toContain('`actual_budget`');
    const budgetMonth = errorOf((await callTool(host, 'ezb_spending_by_category', { month: '2026-08' })).envelope);
    expect(budgetMonth.hint).toContain('`actual_budget`');
    const qb = errorOf((await callTool(host, 'ezb_list_accounts', { realm_id: '1' })).envelope);
    expect(qb.hint).toContain('`quickbooks`');
    const act3 = errorOf((await callTool(host, 'ezb_get_account', { account_id: 'shot_12' })).envelope);
    expect(act3.hint).toContain('`act3`');
    expect(plane.calls.length).toBe(0);
  });

  it('every envelope, success or failure, says app: ezbookkeeping (T3 defence in depth)', async () => {
    const { host } = makeHost();
    const ok = await callTool(host, 'ezb_whoami', {});
    expect((ok.envelope.meta as { app: string }).app).toBe('ezbookkeeping');
    const bad = await callTool(host, 'ezb_get_account', {});
    expect((bad.envelope.meta as { app: string }).app).toBe('ezbookkeeping');
  });

  it('layer 7: the vocabulary is ezBookkeeping\'s, never a budget month', () => {
    const schemas = JSON.stringify(TOOLS.map(t => t.inputSchema.properties));
    expect(schemas).not.toMatch(/"to_budget"|"carryover"|"budget_month"|"payee_id"/);
    expect(schemas).toMatch(/category_ids/);
    expect(schemas).toMatch(/tag_ids/);
    expect(schemas).toMatch(/convert_to/);
  });
});
