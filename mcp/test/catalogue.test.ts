/**
 * Catalogue tests — pm/mcp.mdx §9, §17 "Catalogue tests" and "Route parity", §19 AC 7, 7a, 20, 21, 22.
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { ERROR_CODES, MCP_ONLY_CODES, PLANE_CODES } from '../src/envelope.js';
import { FAMILIES, findTool, READ_TOOLS, TOOLS, TOTAL_TOOLS, WRITE_TOOL_COUNT } from '../src/tools/registry.js';
import { BARE_NAMES, READ_VERBS, READS_ONLY, WHICH_SERVER, WRITE_VERBS, WRITES } from '../src/tools/tool.js';

const here = path.dirname(fileURLToPath(import.meta.url));

type PlaneRoute = { method: string; path: string; tier: string; status: string };
const PLANE = JSON.parse(fs.readFileSync(path.join(here, 'fixtures', 'plane_routes.json'), 'utf8')) as { errorCodes: string[]; routes: PlaneRoute[] };

/**
 * pm/mcp.mdx §9.5 "What this catalogue deliberately does NOT have" — every live read/write route of
 * the plane that has no tool, with its reason. A route not listed here and not covered by a tool
 * fails the parity test.
 */
const OMISSIONS: Record<string, string> = {
  'GET /ping': 'ezb_whoami and ezb_health answer more; ping is the CLI bring-up gate',
  'GET /user/profile': 'ezb_whoami reports the bound user, default currency and first day of week',
  'PATCH /user/profile': 'identity-adjacent settings (nickname, default currency, fiscal year) are the operator\'s to change in the UI',
  'GET /user/settings': 'the web UI\'s own synchronised settings are not the books',
  'PATCH /user/settings': 'the web UI\'s own synchronised settings are not the books',
  'GET /data/export': 'ezb_export_transactions covers the spreadsheet case; a whole-books export is `ezbk data export`',
  'GET /accounts/:id': 'covered by ezb_get_account (listed for completeness of the GET parity)',
  'POST /accounts/batch': 'ezb_apply_accounts provisions in batch from a manifest; one-off accounts are ezb_add_account',
  'POST /accounts/:id/hide': 'hiding is a UI action; §9.5 keeps hide-with-cascade off the agent surface',
  'POST /accounts/:id/move': 'display order is a UI action',
  'GET /transactions/earliest': 'ezb_get_account_properties carries first/last transaction',
  'GET /transactions/latest': 'ezb_get_account_properties carries first/last transaction',
  'POST /transactions/tags/clear': 'ezb_remove_transaction_tags names the tags it removes; a blanket clear is a UI action',
  'POST /transactions/move-all': 'moving every transaction of an account is `ezbk transactions move-all`; ezb_set_transaction_account moves a chosen set',
  'GET /categories/:id': 'ezb_list_categories with name or parent_id answers it',
  'POST /categories/batch': 'the default-categories preset is a first-run UI action',
  'PATCH /categories/:id': 'renaming and re-parenting categories is a UI action',
  'POST /categories/:id/hide': 'hiding is a UI action',
  'POST /categories/:id/move': 'display order is a UI action',
  'GET /tags/:id': 'ezb_list_tags with name answers it',
  'PATCH /tags/:id': 'renaming tags is a UI action',
  'POST /tags/:id/hide': 'hiding is a UI action',
  'POST /tags/:id/move': 'display order is a UI action',
  'POST /tags/batch': 'ezb_add_tag adds one; bulk tag creation is `ezbk tags add`',
  'GET /tag-groups/:id': 'ezb_list_tag_groups with name answers it',
  'POST /tag-groups': 'tag groups are a UI organisation feature',
  'PATCH /tag-groups/:id': 'tag groups are a UI organisation feature',
  'POST /tag-groups/:id/move': 'display order is a UI action',
  'GET /templates/:id': 'ezb_list_templates with name answers it',
  'POST /templates/:id/hide': 'hiding does not pause a schedule; ezb_update_scheduled_transaction with frequency disabled does',
  'POST /templates/:id/move': 'display order is a UI action',
  'DELETE /exchange-rates/custom/:currency': 'removing a custom rate restores the provider\'s; it is `ezbk rates unset`',
  'GET /insights/:id': 'ezb_list_saved_insights with name answers it',
  'POST /insights': 'saved insights are the UI\'s saved queries; the numbers come from the analytics tools',
  'PATCH /insights/:id': 'saved insights are the UI\'s saved queries',
  'POST /insights/:id/hide': 'hiding is a UI action',
  'POST /insights/:id/move': 'display order is a UI action',
  'POST /redo': 'a redo is a second decision; the operator types `ezbk redo` (§9.5)',
  'POST /batch': 'each write is reported and verified before the next is proposed (§18.2)',
  'GET /batch/ops': 'the batch route is not exposed',
  'GET /ingest/roots': 'ezb_whoami reports the statements root; ezb_get_statement_manifest reads it',
  'GET /ingest/converters': 'ezb_capabilities and the plan report the converters in play',
  'PUT /ingest/map': 'ezb_apply_accounts writes the map; hand-editing it is `ezbk statements map`',
  'POST /ingest/map/infer': 'ezb_plan_accounts proposes the mappings with its reasoning',
  'GET /ingest/runs': 'ezb_list_journal and ezb_list_import_fallout cover the agent\'s questions about past runs',
  'GET /ingest/runs/:id': 'as above',
};

describe('the catalogue', () => {
  it('is sixty-five tools: forty-seven read, eighteen write', () => {
    expect(TOTAL_TOOLS).toBe(65);
    expect(READ_TOOLS).toBe(47);
    expect(WRITE_TOOL_COUNT).toBe(18);
    expect(TOOLS.length).toBe(65);
  });

  it('has the families of §9.5 with their sizes', () => {
    const sizes = Object.fromEntries(FAMILIES.map(f => [f.name, f.tools.length]));
    expect(sizes).toEqual({ orientation: 4, accounts: 5, transactions: 4, reference: 6, currency: 2, analytics: 13, statements: 11, previews: 2, writes: 18 });
  });

  it('names every tool ^ezb_[a-z_]+$ with a verb from the closed set (or a sanctioned bare name)', () => {
    for (const tool of TOOLS) {
      expect(tool.name).toMatch(/^ezb_[a-z_]+$/);
      const rest = tool.name.slice('ezb_'.length);
      const readVerb = READ_VERBS.some(v => rest.startsWith(v));
      const writeVerb = WRITE_VERBS.some(v => rest.startsWith(v));
      const bare = (BARE_NAMES as readonly string[]).includes(tool.name);
      const analyticsName = FAMILIES.find(f => f.name === 'analytics')?.tools.includes(tool) === true;
      expect(readVerb || writeVerb || bare || analyticsName, `${tool.name} uses a verb outside the closed set`).toBe(true);
      if (writeVerb) {
        expect(tool.tier, `${tool.name} has a write verb but is not write tier`).toBe('write');
      }
      if (readVerb) {
        expect(tool.tier, `${tool.name} has a read verb but is write tier`).toBe('read');
      }
    }
  });

  it('has no delete_, query_, search_ or generate_ verb', () => {
    for (const tool of TOOLS) {
      expect(tool.name).not.toMatch(/^ezb_(delete|query|search|generate)_/);
    }
  });

  it('has unique names, and list and dispatch agree', () => {
    const names = new Set<string>();
    for (const tool of TOOLS) {
      expect(names.has(tool.name), `${tool.name} is registered twice`).toBe(false);
      names.add(tool.name);
      expect(findTool(tool.name)).toBe(tool);
    }
    expect(findTool('ezb_nope')).toBeUndefined();
  });

  it('carries the four mandatory description clauses on every tool (§9.2)', () => {
    for (const tool of TOOLS) {
      const d = tool.description;
      expect(d.length, `${tool.name} clause 1`).toBeGreaterThan(40);
      expect(d, `${tool.name} clause 2`).toContain(tool.tier === 'read' ? READS_ONLY : WRITES);
      expect(d, `${tool.name} clause 4`).toContain(WHICH_SERVER);
      expect(d).toContain('actual_budget');
      expect(d).toContain('quickbooks');
      expect(d).toContain('act3');
      expect(d.endsWith(WHICH_SERVER), `${tool.name} must end with the which-app clause`).toBe(true);
    }
  });

  it('serves hand-authored JSON Schema objects that refuse unknown keys', () => {
    for (const tool of TOOLS) {
      expect(tool.inputSchema.type).toBe('object');
      expect(tool.inputSchema.additionalProperties).toBe(false);
      expect(typeof tool.inputSchema.properties).toBe('object');
      // The zod schema agrees: an unknown key is refused.
      const parsed = tool.schema.safeParse({ __unknown_key__: 1 });
      expect(parsed.success, `${tool.name} accepted an unknown key`).toBe(false);
    }
  });

  it('types every amount field as integer hundredths with an example (§10.1)', () => {
    const walk = (schema: Record<string, unknown>, where: string): void => {
      const props = schema.properties as Record<string, Record<string, unknown>> | undefined;
      if (props === undefined) {
        return;
      }
      for (const [name, prop] of Object.entries(props)) {
        if (name !== 'hide_amount' && (/(^|_)(amount|balance|credit_card_limit)$/.test(name) || name.endsWith('_amount'))) {
          expect(prop.type, `${where}.${name} must be integer`).toBe('integer');
          expect(String(prop.description), `${where}.${name} must say hundredths`).toMatch(/hundredths/);
          expect(String(prop.description), `${where}.${name} must give an example`).toMatch(/50000/);
        }
        if (name !== 'latitude' && name !== 'longitude') {
          expect(prop.type, `${where}.${name} must never be a float`).not.toBe('number'); // a coordinate is not money
        }
        if (prop.type === 'object') {
          walk(prop, `${where}.${name}`);
        }
        if (prop.type === 'array' && typeof prop.items === 'object' && prop.items !== null) {
          walk(prop.items as Record<string, unknown>, `${where}.${name}[]`);
        }
      }
    };
    for (const tool of TOOLS) {
      walk(tool.inputSchema, tool.name);
    }
  });

  it('never accepts reimport_deleted (§11.3, AC 20), and no property mentions it', () => {
    for (const tool of TOOLS) {
      expect(JSON.stringify(tool.inputSchema)).not.toContain('reimport_deleted');
      const parsed = tool.schema.safeParse({ reimport_deleted: true });
      expect(parsed.success, `${tool.name} accepted reimport_deleted`).toBe(false);
    }
  });

  it('gives every write but ezb_undo a dry_run defaulting to true, and ezb_undo none (AC 21)', () => {
    for (const tool of TOOLS.filter(t => t.tier === 'write')) {
      const props = tool.inputSchema.properties as Record<string, unknown>;
      if (tool.name === 'ezb_undo') {
        expect(tool.hasDryRun).toBe(false);
        expect(props.dry_run).toBeUndefined();
        expect(props.confirm_token).toBeUndefined();
        continue;
      }
      expect(tool.hasDryRun, `${tool.name}`).toBe(true);
      expect(props.dry_run).toBeDefined();
      expect(props.confirm_token).toBeDefined();
      expect(props.max_changes).toBeDefined();
      expect(String((props.dry_run as Record<string, unknown>).description)).toMatch(/default/i);
    }
    for (const tool of TOOLS.filter(t => t.tier === 'read')) {
      const props = tool.inputSchema.properties as Record<string, unknown>;
      expect(props.dry_run, `${tool.name} is read tier and must not carry dry_run`).toBeUndefined();
      expect(props.confirm_token, `${tool.name} is read tier and must not carry confirm_token`).toBeUndefined();
    }
  });
});

describe('route parity with the machine plane (§9, AC 7a)', () => {
  const planeKeys = new Set(PLANE.routes.map(r => `${r.method} ${r.path}`));

  it('maps every tool to exactly one live plane route that is not admin', () => {
    for (const tool of TOOLS) {
      const key = `${tool.route.method} ${tool.route.path}`;
      const route = PLANE.routes.find(r => `${r.method} ${r.path}` === key);
      expect(route, `${tool.name} names ${key}, which the plane does not publish`).toBeDefined();
      expect(route?.status, `${tool.name}: ${key} is not live`).toBe('live');
      expect(route?.tier, `${tool.name}: ${key} is admin tier`).not.toBe('admin');
      expect(route?.tier, `${tool.name}: tool tier disagrees with the plane's`).toBe(tool.route.path === '/ingest/extract' ? 'read' : tool.tier);
    }
  });

  it('never names the passthrough or an admin route', () => {
    for (const tool of TOOLS) {
      expect(tool.route.path.startsWith('/api/')).toBe(false);
      expect(tool.route.path.startsWith('/admin/')).toBe(false);
      expect(tool.route.method).not.toBe('DELETE');
    }
  });

  it('covers every live non-admin plane route with a tool or an omission row', () => {
    const covered = new Set(TOOLS.map(t => `${t.route.method} ${t.route.path}`));
    const missing: string[] = [];
    for (const r of PLANE.routes) {
      const key = `${r.method} ${r.path}`;
      if (r.tier === 'admin' || r.status !== 'live' || r.path.startsWith('/api/')) {
        continue;
      }
      if (!covered.has(key) && OMISSIONS[key] === undefined) {
        missing.push(key);
      }
    }
    expect(missing, 'plane routes with neither a tool nor an omission reason').toEqual([]);
  });

  it('lists no omission that a tool already covers, and none the plane does not have', () => {
    const covered = new Set(TOOLS.map(t => `${t.route.method} ${t.route.path}`));
    for (const key of Object.keys(OMISSIONS)) {
      if (key === 'GET /accounts/:id') {
        continue; // documented twice on purpose: the omission text says so
      }
      expect(covered.has(key), `${key} is listed as an omission but has a tool`).toBe(false);
      expect(planeKeys.has(key), `${key} is listed as an omission but the plane does not publish it`).toBe(true);
    }
  });
});

describe('the error vocabulary (§12.3, AC 22)', () => {
  it('is twelve codes: the plane\'s nine plus three minted here', () => {
    expect(ERROR_CODES.length).toBe(12);
    expect(PLANE_CODES.length).toBe(9);
    expect(MCP_ONLY_CODES.length).toBe(3);
    for (const c of PLANE.errorCodes) {
      expect(ERROR_CODES as readonly string[]).toContain(c);
    }
    expect([...PLANE_CODES].sort()).toEqual([...PLANE.errorCodes].sort());
    for (const c of MCP_ONLY_CODES) {
      expect(PLANE.errorCodes).not.toContain(c);
    }
  });
});
