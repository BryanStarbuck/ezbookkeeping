/**
 * The live integration suite — pm/mcp.mdx §17 "Integration", "Determinism", §18 UC-6/7/9; AC 1, 4,
 * 8, 9, 10, 11, 12, 14, 21, 21a, 21b.
 *
 * A real ezBookkeeping server on an ephemeral port with a temp work dir, temp SQLite, temp
 * credentials, one synthetic user and the synthetic statements tree from pkg/machine/testdata/ingest,
 * driven end to end through the BUILT server over stdio. Needs the Go binary (EZBK_SERVER_BIN or
 * <repo>/ezbookkeeping) and `npm run build`; skips with a reason otherwise.
 *
 * EZBKMCP_RECORD_FIXTURES=1 writes every envelope to test/fixtures/envelopes/ for the currency and
 * redaction canaries (temp paths replaced by {ROOT}).
 */
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { startLiveServer } from './helpers/live.js';
import type { LiveServer } from './helpers/live.js';
import { DIST_ENTRY, MCP_ROOT, StdioRpcClient } from './helpers/rpc.js';

const REPO_ROOT = path.resolve(MCP_ROOT, '..');
const SYNTHETIC_TREE = path.join(REPO_ROOT, 'pkg', 'machine', 'testdata', 'ingest', 'prepared');
const FIXTURES = path.join(MCP_ROOT, 'test', 'fixtures', 'envelopes');
const RECORD = process.env.EZBKMCP_RECORD_FIXTURES === '1';

let live: LiveServer | null = null;
let reason = 'no ezBookkeeping binary with the machine plane (set EZBK_SERVER_BIN), or dist/ not built';
let rw: StdioRpcClient | null = null; // EZBKMCP_ALLOW_WRITE=1
let ro: StdioRpcClient | null = null; // the default: writes off
let statementsRoot = '';
const ids: Record<string, string> = {};
const transcript: string[] = [];

type Env = Record<string, unknown>;

function data(env: Env): Record<string, unknown> {
  return env.data as Record<string, unknown>;
}
function error(env: Env): { code: string; message: string; hint?: string; details?: unknown } {
  return env.error as { code: string; message: string; hint?: string; details?: unknown };
}

async function call(client: StdioRpcClient, tool: string, args: Record<string, unknown> = {}, label?: string): Promise<Env> {
  const { envelope } = await client.call(tool, args, 120_000);
  transcript.push(`${tool} ${JSON.stringify(args).slice(0, 80)} -> ok=${String(envelope.ok)}${envelope.ok === true ? '' : ` ${error(envelope).code}`}`);
  if (RECORD) {
    fs.mkdirSync(FIXTURES, { recursive: true });
    const name = `${label ?? tool}.json`;
    const text = JSON.stringify({ tool, args, envelope }, null, 2).replaceAll(statementsRoot, '{ROOT}').replaceAll(live?.workDir ?? '\u0000', '{WORK}').replaceAll(live?.credentialsFile ?? '\u0000', '{CREDENTIALS}').replaceAll(REPO_ROOT, '{REPO}');
    fs.writeFileSync(path.join(FIXTURES, name), `${text}\n`);
  }
  return envelope;
}

/** dry run → confirm_token → apply; returns both envelopes. */
async function write(client: StdioRpcClient, tool: string, args: Record<string, unknown>, label?: string): Promise<{ preview: Env; applied: Env }> {
  const preview = await call(client, tool, args, label === undefined ? undefined : `${label}.preview`);
  expect(preview.ok, `${tool} preview: ${JSON.stringify(preview.error)}`).toBe(true);
  expect(data(preview).dry_run).toBe(true);
  const token = data(preview).confirm_token as string;
  expect(token).toMatch(/^cf_[0-9a-f]{32}$/);
  const applied = await call(client, tool, { ...args, dry_run: false, confirm_token: token }, label === undefined ? undefined : `${label}.apply`);
  expect(applied.ok, `${tool} apply: ${JSON.stringify(applied.error)}`).toBe(true);
  expect(data(applied).dry_run).toBe(false);
  return { preview, applied };
}

describe('live integration (a real server, the built MCP, synthetic books)', () => {
  beforeAll(async () => {
    if (!fs.existsSync(DIST_ENTRY)) {
      reason = 'dist/index.js is not built; run `npm run build`';
      return;
    }
    try {
      live = await startLiveServer({ allowWrite: true });
    } catch (err) {
      reason = (err as Error).message;
      live = null;
    }
    if (live === null) {
      return;
    }
    // The synthetic statements tree, copied OUT of the repo into a temp dir (it is the fixture the
    // Go ingest tests use: invented entities, invented banks, invented last-fours).
    statementsRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'ezbkmcp-statements-'));
    fs.cpSync(SYNTHETIC_TREE, statementsRoot, { recursive: true });
    rw = new StdioRpcClient(live.mcpEnv({ EZBKMCP_ALLOW_WRITE: '1', EZBK_STATEMENTS_DIR: statementsRoot }));
    ro = new StdioRpcClient(live.mcpEnv({ EZBK_STATEMENTS_DIR: statementsRoot }));
    await rw.initialize();
    await ro.initialize();
  }, 120_000);

  afterAll(async () => {
    await rw?.close();
    await ro?.close();
    await live?.stop();
    if (statementsRoot !== '') {
      fs.rmSync(statementsRoot, { recursive: true, force: true });
    }
    if (transcript.length > 0) {
      process.stderr.write(`\nlive transcript (${String(transcript.length)} calls):\n  ${transcript.join('\n  ')}\n`);
    }
  });

  const needsLive = (skip: (r: string) => void): boolean => {
    if (live === null || rw === null || ro === null) {
      skip(reason);
      return false;
    }
    return true;
  };

  it('spine: initialize names the server, lists 65 tools, and the banner shows only a fingerprint (AC 1, 4)', async ({ skip }) => {
    if (!needsLive(skip)) return;
    const tools = await rw!.listTools();
    expect(tools.length).toBe(65);
    expect(tools.every(t => t.name.startsWith('ezb_'))).toBe(true);
    const stderr = rw!.stderr.join('');
    expect(stderr).toContain('writes ENABLED');
    expect(stderr).not.toContain(live!.key);
    expect(ro!.stderr.join('')).toContain('writes off');
  });

  it('orientation: whoami binds the synthetic user; health is healthy; capabilities and the data summary answer', async ({ skip }) => {
    if (!needsLive(skip)) return;
    const who = await call(rw!, 'ezb_whoami');
    expect(who.ok).toBe(true);
    expect((who.meta as { user: string; app: string; defaultCurrency: string }).user).toBe('operator');
    expect((who.meta as { app: string }).app).toBe('ezbookkeeping');
    expect((who.meta as { defaultCurrency: string }).defaultCurrency).toBe('USD');
    expect((data(who).user as { username: string }).username).toBe('operator');
    expect(data(who).keyFingerprint).toMatch(/^[0-9a-f]{4}…\/sha256:[0-9a-f]{4}$/);
    expect((data(who).mcp as { allowWrite: boolean }).allowWrite).toBe(true);
    const health = await call(rw!, 'ezb_health');
    expect(data(health).healthy).toBe(true);
    expect(data(health).upstreamDoors).toEqual({ apiToken: false, mcp: false });
    const caps = await call(rw!, 'ezb_capabilities');
    expect((data(caps).tiers as { write: boolean }).write).toBe(true);
    const summary = await call(rw!, 'ezb_get_data_summary');
    expect(summary.ok).toBe(true);
  });

  it('writes are off by default: the read-only server lists but refuses every write tool (AC 8)', async ({ skip }) => {
    if (!needsLive(skip)) return;
    const tools = await ro!.listTools();
    const disabled = tools.filter(t => t.description.includes('CURRENTLY DISABLED'));
    expect(disabled.length).toBe(18);
    const refused = await call(ro!, 'ezb_add_tag', { name: 'nope' });
    expect(error(refused).code).toBe('write_disabled');
    expect(error(refused).hint).toContain('EZBKMCP_ALLOW_WRITE=1');
  });

  it('seeds the books through the write protocol: accounts, transactions, a tag (UC-9 shape)', async ({ skip }) => {
    if (!needsLive(skip)) return;
    const checking = await write(rw!, 'ezb_add_account', { name: 'Northbank Checking', category: 'checking', currency: 'USD', initial_balance: 250000, initial_balance_date: '2026-01-01' }, 'ezb_add_account');
    ids.checking = (data(checking.applied).result as { account: { id: string } }).account.id;
    const savings = await write(rw!, 'ezb_add_account', { name: 'Meridian Savings', category: 'savings', currency: 'USD', initial_balance: 1000000, initial_balance_date: '2026-01-01' });
    ids.savings = (data(savings.applied).result as { account: { id: string } }).account.id;
    const card = await write(rw!, 'ezb_add_account', { name: 'Euro Card', category: 'credit_card', currency: 'EUR', initial_balance: -30000, initial_balance_date: '2026-01-01' });
    ids.card = (data(card.applied).result as { account: { id: string } }).account.id;
    expect((data(card.preview).preview as { side: string }).side).toBe('liability');

    const tag = await write(rw!, 'ezb_add_tag', { name: 'Trip' }, 'ezb_add_tag');
    ids.trip = (data(tag.applied).result as { tag: { id: string } }).tag.id;

    const txns = await write(
      rw!,
      'ezb_add_transactions',
      {
        transactions: [
          { type: 'income', date: '2026-07-01', account_name: 'Northbank Checking', amount: 500000, category_name: 'Paycheck', comment: 'ACME PAYROLL' },
          { type: 'expense', date: '2026-07-03', account_name: 'Northbank Checking', amount: 12350, category_name: 'Groceries', comment: 'NORTHSIDE MARKET' },
          { type: 'expense', date: '2026-07-15', account_name: 'Northbank Checking', amount: 18900, category_name: 'Other', comment: 'ACME HARDWARE' },
          { type: 'income', date: '2026-08-01', account_name: 'Northbank Checking', amount: 500000, category_name: 'Paycheck', comment: 'ACME PAYROLL' },
          { type: 'expense', date: '2026-08-02', account_name: 'Northbank Checking', amount: 13100, category_name: 'Groceries', comment: 'NORTHSIDE MARKET' },
          { type: 'transfer', date: '2026-08-05', account_name: 'Northbank Checking', amount: 100000, destination_account_name: 'Meridian Savings', destination_amount: 100000, category_name: 'General Transfer', comment: 'monthly savings' },
          { type: 'expense', date: '2026-08-14', account_name: 'Northbank Checking', amount: 7600, category_name: 'Other', comment: 'ACME HARDWARE' },
          { type: 'expense', date: '2026-08-20', account_name: 'Euro Card', amount: 8250, category_name: 'Restaurants', comment: 'LISBON BISTRO', tag_names: ['Trip'] },
          { type: 'expense', date: '2026-09-02', account_name: 'Northbank Checking', amount: 12800, category_name: 'Groceries', comment: 'NORTHSIDE MARKET' },
        ],
      },
      'ezb_add_transactions',
    );
    expect((data(txns.preview).changes as { create: number }).create).toBe(9);
    const created = (data(txns.applied).result as { created: number; ids: string[] }).created;
    expect(created).toBe(9);
    ids.txn = (data(txns.applied).result as { ids: string[] }).ids[0] ?? '';
  });

  it('reads: accounts, one account by name, balance as of, properties, list/count/export, the reference data (AC 10, 12)', async ({ skip }) => {
    if (!needsLive(skip)) return;
    const accounts = await call(ro!, 'ezb_list_accounts', {}, 'ezb_list_accounts');
    expect((data(accounts).accounts as unknown[]).length).toBe(3);
    for (const a of data(accounts).accounts as Array<{ balance: number; currency: string }>) {
      expect(Number.isInteger(a.balance)).toBe(true);
      expect(a.currency).toMatch(/^[A-Z]{3}$/);
    }
    const one = await call(ro!, 'ezb_get_account', { account_name: 'Euro Card' }, 'ezb_get_account');
    expect((data(one).account as { currency: string; side: string }).side).toBe('liability');
    const bal = await call(ro!, 'ezb_get_account_balance', { account_id: ids.checking ?? '', as_of: '2026-08-31' }, 'ezb_get_account_balance');
    expect(data(bal)).toMatchObject({ currency: 'USD', asOf: '2026-08-31' });
    expect(Number.isInteger(data(bal).balance)).toBe(true);
    const props = await call(ro!, 'ezb_get_account_properties', { account_name: 'Northbank Checking' }, 'ezb_get_account_properties');
    expect(props.ok).toBe(true);
    const recon = await call(ro!, 'ezb_get_reconciliation_statement', { account_name: 'Northbank Checking', start: '2026-08-01', end: '2026-08-31' }, 'ezb_get_reconciliation_statement');
    expect(data(recon)).toMatchObject({ currency: 'USD' });

    const list = await call(ro!, 'ezb_list_transactions', { keyword: 'ACME HARDWARE' }, 'ezb_list_transactions');
    expect((data(list).transactions as unknown[]).length).toBe(2);
    expect((list.meta as { untrusted: string[] }).untrusted).toContain('comment');
    const count = await call(ro!, 'ezb_count_transactions', { start: '2026-07-01' }, 'ezb_count_transactions');
    expect(data(count).count).toBe(9);
    const one2 = await call(ro!, 'ezb_get_transaction', { transaction_id: ids.txn ?? '' }, 'ezb_get_transaction');
    expect(one2.ok).toBe(true);
    const csv = await call(ro!, 'ezb_export_transactions', { start: '2026-09-01', format: 'csv' }, 'ezb_export_transactions');
    expect(String(data(csv).content)).toContain('NORTHSIDE MARKET');
    const page = await call(ro!, 'ezb_list_transactions', { limit: 2 });
    expect((page.meta as { truncated: boolean }).truncated).toBe(true);
    expect(data(page).nextCursor).not.toBeNull();

    for (const [tool, args] of [
      ['ezb_list_categories', { type: 'expense' }],
      ['ezb_list_tags', {}],
      ['ezb_list_tag_groups', {}],
      ['ezb_list_templates', {}],
      ['ezb_list_upcoming_schedules', { days: 30 }],
      ['ezb_list_saved_insights', {}],
      ['ezb_list_journal', { limit: 5 }],
    ] as Array<[string, Record<string, unknown>]>) {
      const env = await call(ro!, tool, args, tool);
      expect(env.ok, tool).toBe(true);
    }
  });

  it('analytics: per-currency without convert_to, rates named with it, and the rest of the family (AC 11)', async ({ skip }) => {
    if (!needsLive(skip)) return;
    const range = { start: '2026-07-01', end: '2026-09-30' };
    const perCurrency = await call(ro!, 'ezb_spending_by_category', range, 'ezb_spending_by_category.per_currency');
    expect(perCurrency.ok).toBe(true);
    const series = data(perCurrency).series as Array<{ currency: string; total: number }>;
    expect(new Set(series.map(s => s.currency)).size).toBeGreaterThanOrEqual(2);
    expect(JSON.stringify(data(perCurrency))).not.toContain('convertedTotal');

    const converted = await call(ro!, 'ezb_spending_by_category', { ...range, convert_to: 'USD', interval: 'month', group_by: 'secondary', top_n: 3 }, 'ezb_spending_by_category.converted');
    if (converted.ok) {
      const json = JSON.stringify(data(converted));
      expect(json).toContain('"rateBasis":"latest"');
      expect(json).toMatch(/"source":"(provider|custom)"/);
    } else {
      // No exchange rates reachable in this environment: the plane says so rather than inventing one.
      expect(['upstream_error', 'forbidden', 'invalid_input', 'not_ready']).toContain(error(converted).code);
    }

    const rest: Array<[string, Record<string, unknown>]> = [
      ['ezb_period_summary', {}],
      ['ezb_income_by_category', range],
      ['ezb_income_vs_expense', { ...range, interval: 'month' }],
      ['ezb_cash_flow', { ...range, interval: 'month' }],
      ['ezb_net_worth', { ...range, interval: 'none' }],
      ['ezb_account_balance_history', { account_id: ids.checking ?? '', ...range }],
      ['ezb_tag_breakdown', { tag_ids: [ids.trip ?? ''], ...range }],
      ['ezb_payee_leaderboard', range],
      ['ezb_list_recurring', { start: '2026-01-01', end: '2026-09-30' }],
      ['ezb_list_anomalies', { start: '2026-01-01', end: '2026-09-30' }],
      ['ezb_get_runway', {}],
    ];
    for (const [tool, args] of rest) {
      const env = await call(ro!, tool, args, tool);
      expect(env.ok, `${tool}: ${JSON.stringify(env.error)}`).toBe(true);
      expect((env.meta as { untrusted?: string[] }).untrusted).toBeDefined();
    }
    const trend = await call(ro!, 'ezb_category_trend', { category_id: firstCategoryId(await call(ro!, 'ezb_list_categories', { type: 'expense', flat: true })), ...range }, 'ezb_category_trend');
    expect(trend.ok).toBe(true);
    const rates = await call(ro!, 'ezb_get_exchange_rates', {}, 'ezb_get_exchange_rates');
    const convert = await call(ro!, 'ezb_convert_amount', { amount: 10000, from: 'EUR', to: 'USD' }, 'ezb_convert_amount');
    if (rates.ok && convert.ok) {
      expect(Number.isInteger(data(convert).converted)).toBe(true);
      expect(data(convert).rateBasis).toBe('latest');
    }
  });

  it('the write protocol live: preview names the rows; no token → confirm_required; stale → conflict; over the ceiling → too_many_changes; undo restores (AC 9, 21a)', async ({ skip }) => {
    if (!needsLive(skip)) return;
    const before = await call(ro!, 'ezb_list_transactions', { keyword: 'ACME HARDWARE' });
    const beforeCats = (data(before).transactions as Array<{ categoryId: string }>).map(t => t.categoryId);

    const noToken = await call(rw!, 'ezb_set_transaction_category', { filter: { keyword: 'ACME HARDWARE' }, category_name: 'Home Repair', dry_run: false });
    expect(error(noToken).code).toBe('confirm_required');

    const stale = await call(rw!, 'ezb_set_transaction_category', { filter: { keyword: 'ACME HARDWARE' }, category_name: 'Home Repair', dry_run: false, confirm_token: 'cf_00000000000000000000000000000000' });
    expect(error(stale).code).toBe('conflict');

    const ceiling = await call(rw!, 'ezb_set_transaction_category', { filter: { keyword: 'ACME HARDWARE', type: 'expense' }, category_name: 'Home Repair', max_changes: 1 });
    expect(error(ceiling).code).toBe('too_many_changes');
    expect(error(ceiling).message).toContain('2 changes');

    const recat = await write(rw!, 'ezb_set_transaction_category', { filter: { keyword: 'ACME HARDWARE' }, category_name: 'Home Repair' }, 'ezb_set_transaction_category');
    expect((data(recat.preview).changes as { update: number }).update).toBe(2);
    expect((data(recat.preview).preview as { rows: unknown[] }).rows.length).toBe(2);
    const after = await call(ro!, 'ezb_list_transactions', { keyword: 'ACME HARDWARE' });
    expect((data(after).transactions as Array<{ categoryId: string }>).map(t => t.categoryId)).not.toEqual(beforeCats);

    const undone = await call(rw!, 'ezb_undo', {}, 'ezb_undo');
    expect(undone.ok).toBe(true);
    expect((data(undone).entry as { route: string; undone: boolean }).route).toBe('POST /transactions/set-category');
    const restored = await call(ro!, 'ezb_list_transactions', { keyword: 'ACME HARDWARE' });
    expect((data(restored).transactions as Array<{ categoryId: string }>).map(t => t.categoryId)).toEqual(beforeCats);

    // ezb_undo is refused on the read-only server too
    expect(error(await call(ro!, 'ezb_undo', {})).code).toBe('write_disabled');
  });

  it('the remaining writes each preview then apply: tags, update, move, category, schedule, custom rate, reconcile', async ({ skip }) => {
    if (!needsLive(skip)) return;
    const txn = ids.txn ?? '';
    await write(rw!, 'ezb_add_transaction_tags', { ids: [txn], tag_names: ['Trip'] }, 'ezb_add_transaction_tags');
    await write(rw!, 'ezb_remove_transaction_tags', { ids: [txn], tag_ids: [ids.trip ?? ''] }, 'ezb_remove_transaction_tags');
    const upd = await write(rw!, 'ezb_update_transaction', { transaction_id: txn, comment: 'ACME PAYROLL SEPT', amount: 500100 }, 'ezb_update_transaction');
    expect(JSON.stringify(data(upd.preview).preview)).toContain('"field":"sourceAmount"');
    await write(rw!, 'ezb_set_transaction_account', { ids: [txn], account_name: 'Northbank Checking' }, 'ezb_set_transaction_account');
    const acct = await write(rw!, 'ezb_add_account', { name: 'Yen Cash', category: 'cash', currency: 'JPY', initial_balance: 500000, initial_balance_date: '2026-09-01' });
    const yen = (data(acct.applied).result as { account: { id: string } }).account.id;
    await write(rw!, 'ezb_update_account', { account_id: yen, comment: 'wallet' }, 'ezb_update_account');
    const immutable = await call(rw!, 'ezb_update_account', { account_id: yen, name: 'x', dry_run: true });
    expect(immutable.ok).toBe(true);
    await write(rw!, 'ezb_add_category', { name: 'Travel', type: 'expense' }, 'ezb_add_category');
    await write(rw!, 'ezb_add_category', { name: 'Flights', parent_name: 'Travel' });
    const sched = await write(rw!, 'ezb_add_scheduled_transaction', { name: 'Rent', type: 'expense', account_name: 'Northbank Checking', amount: 240000, category_name: 'Other', frequency: 'monthly', frequency_value: [1], start: '2026-10-01', comment: 'RENT' }, 'ezb_add_scheduled_transaction');
    const schedId = (data(sched.applied).result as { template: { id: string } }).template.id;
    const upcoming = await call(ro!, 'ezb_list_upcoming_schedules', { days: 60 }, 'ezb_list_upcoming_schedules');
    expect((data(upcoming).occurrences as unknown[]).length).toBeGreaterThan(0);
    await write(rw!, 'ezb_update_scheduled_transaction', { template_id: schedId, frequency: 'disabled' }, 'ezb_update_scheduled_transaction');
    await write(rw!, 'ezb_set_custom_exchange_rate', { currency: 'EUR', rate: '0.93' }, 'ezb_set_custom_exchange_rate');

    // Reconcile: the plan with a difference (mark_reconciled false: the synthetic user has last-reconciled-time off), then apply with the token
    const plan = await call(ro!, 'ezb_plan_reconcile', { account_name: 'Meridian Savings', target_balance: 1100500, as_of: '2026-09-21', mark_reconciled: false, create_adjustment: true, adjustment_category_name: 'Paycheck' }, 'ezb_plan_reconcile');
    expect(plan.ok, JSON.stringify(plan.error)).toBe(true);
    expect(data(plan).difference).toBe(500);
    const token = data(plan).confirm_token as string;
    expect(token).toMatch(/^cf_/);
    const applied = await call(rw!, 'ezb_apply_reconcile', { account_name: 'Meridian Savings', target_balance: 1100500, as_of: '2026-09-21', mark_reconciled: false, create_adjustment: true, adjustment_category_name: 'Paycheck', dry_run: false, confirm_token: token }, 'ezb_apply_reconcile');
    expect(applied.ok, JSON.stringify(applied.error)).toBe(true);
    const undone = await call(rw!, 'ezb_undo', {});
    expect((data(undone).entry as { route: string }).route).toContain('reconcile/apply');
  });

  it('statements: manifest first, scan, gaps, dupes, rows, map; plan_accounts → apply_accounts; plan twice identical → apply → zero new; undo (UC-6, UC-7, AC 14, 21b)', async ({ skip }) => {
    if (!needsLive(skip)) return;
    const manifest = await call(ro!, 'ezb_get_statement_manifest', {}, 'ezb_get_statement_manifest');
    expect(manifest.ok, JSON.stringify(manifest.error)).toBe(true);
    expect(data(manifest).mode).toBe('prepared');
    expect((data(manifest).accounts as unknown[]).length).toBe(3);
    expect(JSON.stringify(manifest)).not.toContain('/Users/');

    for (const [tool, args] of [
      ['ezb_scan_statements', {}],
      ['ezb_list_missing_statements', {}],
      ['ezb_list_statement_duplicates', {}],
      ['ezb_describe_statement_map', {}],
      ['ezb_get_statement_rows', { account: 'Checking_x4021' }],
    ] as Array<[string, Record<string, unknown>]>) {
      const env = await call(ro!, tool, args, tool);
      expect(env.ok, `${tool}: ${JSON.stringify(env.error)}`).toBe(true);
    }
    const rows = await call(ro!, 'ezb_get_statement_rows', { account: 'Checking_x4021' });
    expect(JSON.stringify(data(rows))).not.toMatch(/"(rawText|raw_text|document)":/);

    const extract = await call(ro!, 'ezb_extract_statements', {});
    expect(extract.ok === true || ['invalid_input', 'conflict'].includes(error(extract).code)).toBe(true);

    const plan = await call(ro!, 'ezb_plan_accounts', {}, 'ezb_plan_accounts');
    expect(plan.ok, JSON.stringify(plan.error)).toBe(true);
    const summary = data(plan).summary as { create: number; link: number; ambiguous: number };
    expect(summary.create).toBe(3);
    expect(summary.ambiguous).toBe(0);
    for (const row of data(plan).plan as Array<{ action: string; proposed?: { side: string; currency: string; category: string } }>) {
      if (row.action === 'create') {
        expect(row.proposed?.currency).toMatch(/^[A-Z]{3}$/);
        expect(['asset', 'liability']).toContain(row.proposed?.side);
      }
    }
    const card = (data(plan).plan as Array<{ manifest: { kind: string }; proposed?: { category: string; side: string } }>).find(r => r.manifest.kind === 'card');
    expect(card?.proposed).toMatchObject({ category: 'credit_card', side: 'liability' });

    const applyNoToken = await call(rw!, 'ezb_apply_accounts', { dry_run: false });
    expect(error(applyNoToken).code).toBe('confirm_required');
    const applied = await call(rw!, 'ezb_apply_accounts', { dry_run: false, confirm_token: data(plan).confirm_token as string }, 'ezb_apply_accounts');
    expect(applied.ok, JSON.stringify(applied.error)).toBe(true);

    // A re-run yields link, never duplicates (§11.5, AC 21b)
    const again = await call(ro!, 'ezb_plan_accounts', {});
    expect((data(again).summary as { create: number; link: number }).link).toBe(3);
    expect((data(again).summary as { create: number }).create).toBe(0);

    const cats = await call(ro!, 'ezb_list_categories', { flat: true });
    const expense = (data(cats).categories as Array<{ id: string; level: string; type?: string; name: string }>).find(c => c.name === 'Other');
    const income = (data(cats).categories as Array<{ id: string; level: string; name: string }>).find(c => c.name === 'Paycheck');
    const fallback = { expense: expense?.id ?? '', income: income?.id ?? '' };

    const p1 = await call(ro!, 'ezb_plan_statement_import', { fallback_category_ids: fallback }, 'ezb_plan_statement_import');
    expect(p1.ok, JSON.stringify(p1.error)).toBe(true);
    const p2 = await call(ro!, 'ezb_plan_statement_import', { fallback_category_ids: fallback });
    const strip = (e: Env): unknown => {
      const d = { ...data(e) };
      delete d.confirm_token;
      delete d.expires_at;
      return d;
    };
    expect(strip(p1)).toEqual(strip(p2)); // determinism
    const totalsOf = (e: Env): { new: number } => (data(e).plan as { totals: { new: number } }).totals;
    const totals = totalsOf(p1);
    expect(totals.new).toBeGreaterThan(0);
    // The QIF account is blocked because the plane refuses to guess a date order (§14.2), and says so.
    const blocked = (data(p1).plan as { accounts: Array<{ blocked: boolean; blocked_reasons: string[] }> }).accounts.filter(a => a.blocked);
    expect(blocked.some(a => a.blocked_reasons.some(r => r.includes('qif_date_order')))).toBe(true);

    const imported = await call(rw!, 'ezb_apply_statement_import', { fallback_category_ids: fallback, dry_run: false, confirm_token: data(p2).confirm_token as string }, 'ezb_apply_statement_import');
    expect(imported.ok, JSON.stringify(imported.error)).toBe(true);

    const p3 = await call(ro!, 'ezb_plan_statement_import', { fallback_category_ids: fallback }, 'ezb_plan_statement_import.after');
    expect(totalsOf(p3).new).toBe(0);
    expect((data(p3).plan as { totals: { already_present: number } }).totals.already_present).toBe(totals.new);

    const fallout = await call(ro!, 'ezb_list_import_fallout', { fallback_category_ids: [fallback.expense, fallback.income], start: '2020-01-01', end: '2026-12-31' }, 'ezb_list_import_fallout');
    expect(fallout.ok, JSON.stringify(fallout.error)).toBe(true);

    const filePlan = await call(ro!, 'ezb_plan_file_import', { account_name: 'Household · Northbank Checking ••4021', path: 'bank/household/Northbank/Checking_x4021/2025/10/20251031-statement-4021.ofx', fallback_category_ids: fallback }, 'ezb_plan_file_import');
    expect(filePlan.ok, JSON.stringify(filePlan.error)).toBe(true);

    // Undo reverses the import: the created rows AND the plane's import records go, so the next
    // plan sees the rows as new again (an undo is not a browser delete).
    const undone = await call(rw!, 'ezb_undo', {});
    expect(undone.ok).toBe(true);
    expect((data(undone).entry as { route: string }).route).toBe('POST /ingest/apply');
    const p4 = await call(ro!, 'ezb_plan_statement_import', { fallback_category_ids: fallback });
    expect(totalsOf(p4).new).toBe(totals.new);

    // Re-import, then delete one imported row IN THE BROWSER (upstream's own delete, as the user's
    // session): the next plan reports it already present but deleted since, and never re-adds it —
    // and there is no argument here that could (§11.3, AC 14, AC 20).
    const reimported = await call(rw!, 'ezb_apply_statement_import', { fallback_category_ids: fallback, dry_run: false, confirm_token: data(p4).confirm_token as string });
    expect(reimported.ok, JSON.stringify(reimported.error)).toBe(true);
    const listed = await call(ro!, 'ezb_list_transactions', { account_names: ['Household · Northbank Checking ••4021'], limit: 1 });
    expect(listed.ok, JSON.stringify(listed.error)).toBe(true);
    const victim = (data(listed).transactions as Array<{ id: string }>)[0]?.id ?? '';
    expect(victim).toMatch(/^\d+$/);
    const del = await live!.browser('POST', '/transactions/delete.json', { id: victim });
    expect(del.status, del.text).toBe(200);
    const p5 = await call(ro!, 'ezb_plan_statement_import', { fallback_category_ids: fallback }, 'ezb_plan_statement_import.after_browser_delete');
    expect(totalsOf(p5).new).toBe(0);
    expect((data(p5).plan as { totals: { already_present_deleted: number } }).totals.already_present_deleted).toBe(1);
    const refused = await call(rw!, 'ezb_apply_statement_import', { fallback_category_ids: fallback, reimport_deleted: true, dry_run: false, confirm_token: data(p5).confirm_token as string });
    expect(error(refused).code).toBe('invalid_input');
  }, 180_000);

  it('the audit file after the session holds no amount, comment, account id or path (AC 16)', ({ skip }) => {
    if (!needsLive(skip)) return;
    const info = fs.readFileSync(path.join(live!.stateDir, 'mcp.info'), 'utf8');
    expect(info).toContain('CALL ezb_whoami');
    for (const forbidden of ['ACME', 'NORTHSIDE', 'LISBON', '12350', '500000', statementsRoot, 'Checking_x4021', ids.checking ?? '\u0000']) {
      expect(info).not.toContain(forbidden);
    }
    expect(info).not.toContain(live!.key);
  });
});

function firstCategoryId(env: Env): string {
  const cats = (env.data as { categories: Array<{ id: string; level: string }> }).categories;
  return cats.find(c => c.level === 'secondary')?.id ?? cats[0]?.id ?? '';
}
