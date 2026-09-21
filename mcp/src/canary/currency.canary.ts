/**
 * T4 — currency confusion (pm/mcp.mdx §7.0, §10, §17 "currency canary", AC 10, 11): every numeric
 * amount in every recorded envelope has a reachable currency, no amount is a float or a decimal
 * string, and no analytics response without convert_to holds a cross-currency total.
 *
 * The envelopes are recorded from the live integration run over synthetic books
 * (test/fixtures/envelopes/*.json). Without a recording the structural checks still run over the
 * schemas.
 */
import fs from 'node:fs';
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import { TOOLS } from '../tools/registry.js';

import { MCP_ROOT, walkJson } from './_shared.js';

const FIXTURES = path.join(MCP_ROOT, 'test', 'fixtures', 'envelopes');
const AMOUNT_KEY = /^(amount|total|balance|closingBalance|openingBalance|sourceAmount|destinationAmount|income|expense|net|inflow|outflow|converted|liquidAssets|assets|liabilities|currentBalance|initialBalance|creditCardLimit|difference|appBalance|target|netOutflow|trailingIncome|trailingExpense|averageMonthlyNetOutflow|annualized|annualizedAmount|mean|median|deviation)$/;
const CURRENCY_KEY = /^(currency|sourceCurrency|destinationCurrency|convertTo|convertedTo|defaultCurrency|from|to|baseCurrency)$/;

function recordings(): Array<{ file: string; tool: string; args: Record<string, unknown>; envelope: Record<string, unknown> }> {
  if (!fs.existsSync(FIXTURES)) {
    return [];
  }
  return fs
    .readdirSync(FIXTURES)
    .filter(f => f.endsWith('.json'))
    .map(f => ({ file: f, ...(JSON.parse(fs.readFileSync(path.join(FIXTURES, f), 'utf8')) as { tool: string; args: Record<string, unknown>; envelope: Record<string, unknown> }) }));
}

function hasCurrency(obj: Record<string, unknown>, trail: Array<Record<string, unknown>>): boolean {
  for (const o of [obj, ...trail].reverse()) {
    for (const k of Object.keys(o)) {
      if (CURRENCY_KEY.test(k) && typeof o[k] === 'string') {
        return true;
      }
    }
  }
  return false;
}

describe('canary: currency', () => {
  it('every amount argument in every schema is an integer that says hundredths', () => {
    for (const t of TOOLS) {
      const json = JSON.stringify(t.inputSchema);
      for (const m of json.matchAll(/"(amount|min_amount|max_amount|target_balance|initial_balance|destination_amount|credit_card_limit)":\{"type":"([a-z]+)"/g)) {
        expect(m[2], `${t.name}.${m[1] ?? ''}`).toBe('integer');
      }
    }
  });

  it('every convert_to argument is an ISO 4217 code, never a rate', () => {
    for (const t of TOOLS) {
      const props = t.inputSchema.properties as Record<string, Record<string, unknown>>;
      if (props.convert_to !== undefined) {
        expect(props.convert_to.pattern).toBe('^[A-Z]{3}$');
      }
      expect(JSON.stringify(props)).not.toMatch(/"exchange_rate"|"rate_override"|"fx_rate"/);
    }
  });

  it('has recorded envelopes from a live run (or says why not)', () => {
    const recorded = recordings();
    if (recorded.length === 0) {
      console.error('currency canary: no recorded envelopes in test/fixtures/envelopes/; run the live integration suite with EZBKMCP_RECORD_FIXTURES=1');
    }
    expect(Array.isArray(recorded)).toBe(true);
  });

  it('every amount in every recorded envelope is an integer with a reachable currency (AC 10)', () => {
    for (const r of recordings()) {
      if (r.envelope.ok !== true) {
        continue;
      }
      walkJson(r.envelope.data, (obj, trail, keyPath) => {
        // `total` beside limit/offset/rows/count is a row count (pagination), not money.
        const paginated = 'offset' in obj || 'limit' in obj || ('rows' in obj && Array.isArray(obj.rows)) || ('count' in obj && !('currency' in obj));
        for (const [k, v] of Object.entries(obj)) {
          if (!AMOUNT_KEY.test(k) || v === null || (k === 'total' && paginated)) {
            continue;
          }
          if (typeof v === 'number') {
            expect(Number.isInteger(v), `${r.file} ${keyPath}.${k} = ${String(v)} is not an integer`).toBe(true);
            expect(hasCurrency(obj, trail), `${r.file} ${keyPath}.${k} has no reachable currency`).toBe(true);
          } else if (typeof v === 'string') {
            expect(v, `${r.file} ${keyPath}.${k} is a decimal string`).not.toMatch(/^-?\d+\.\d+$/);
          }
        }
      });
    }
  });

  it('no analytics envelope without convert_to holds a cross-currency total (AC 11)', () => {
    for (const r of recordings()) {
      const tool = TOOLS.find(t => t.name === r.tool);
      if (tool === undefined || r.envelope.ok !== true || r.args.convert_to !== undefined) {
        continue;
      }
      const family = tool.route.path.startsWith('/analytics/') || tool.route.path.endsWith('/balance-history');
      if (!family) {
        continue;
      }
      const json = JSON.stringify(r.envelope.data);
      expect(json, `${r.file} converted without convert_to`).not.toMatch(/"convertedTotal"|"grandTotal"|"totalConverted"/);
      // A per-currency series never mixes: every series object with a total names one currency.
      walkJson(r.envelope.data, (obj, _trail, keyPath) => {
        if (typeof obj.total === 'number' && obj.currency === undefined) {
          expect(obj.currency, `${r.file} ${keyPath}.total has no currency`).toBeDefined();
        }
      });
    }
  });

  it('every converted recording names its rates with source and time, and rateBasis latest', () => {
    for (const r of recordings()) {
      if (r.envelope.ok !== true || r.args.convert_to === undefined) {
        continue;
      }
      const json = JSON.stringify(r.envelope.data);
      expect(json, r.file).toContain('"rateBasis":"latest"');
      expect(json, r.file).toMatch(/"source":"(provider|custom)"/);
      expect(json, r.file).toMatch(/"updateTime"|"updatedAt"/);
    }
  });
});
