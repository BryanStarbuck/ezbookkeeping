/**
 * Analytics — pm/mcp.mdx §9.5, §10. Thirteen read tools: where every total comes from, so a model
 * never adds two numbers and never multiplies by an exchange rate. Every tool takes the shared
 * arguments (apis.mdx §12.2) and every response echoes its filters, its timezone and — if
 * converted — every rate.
 */
import { z } from 'zod';

import { amountField, boolField, clampLimit, currencyField, dateField, describe, enumField, fromPlane, hundredths, idField, idListField, intField, objectSchema, seg, strField, zCurrency, zDate, zId } from './tool.js';
import type { ToolDef, ToolResult } from './tool.js';

const ANALYTICS_UNTRUSTED = ['label', 'name', 'comment', 'variants', 'text'];

/** The shared arguments (apis.mdx §12.2). */
const COMMON_PROPERTIES: Record<string, unknown> = {
  start: dateField('First day of the range, inclusive. Each tool has its own default when omitted, echoed in the response.'),
  end: dateField('Last day of the range, inclusive.'),
  account_ids: idListField('Only these accounts. Defaults to every visible account.'),
  category_ids: idListField('Only these categories (a primary includes its secondaries).'),
  exclude_category_ids: idListField('Leave these categories out.'),
  tag_ids: idListField('Only transactions carrying any of these tags.'),
  tag_filter: strField("Upstream's tag filter syntax instead of tag_ids: `<mode>:<tagId>,<tagId>` joined by `;`, mode 0 has any, 1 has all, 2 not has any, 3 not has all; or `none` for untagged."),
  include_hidden_accounts: boolField('Include hidden accounts. Defaults to false, and the response says so.'),
  include_transfers: boolField('Count transfers between the operator\'s own accounts. Defaults to false: a transfer is not spending.'),
  convert_to: currencyField('Convert every figure into this currency with the app\'s rates; without it the answer is per currency and no total crosses a currency. Every rate used is named, and historical figures use today\'s rate (rateBasis latest).'),
  use_transaction_timezone: boolField("Bucket each transaction by the timezone it was recorded in rather than the resolved zone. Defaults to false."),
};

const zCommon = {
  start: zDate.optional(),
  end: zDate.optional(),
  account_ids: z.array(zId).optional(),
  category_ids: z.array(zId).optional(),
  exclude_category_ids: z.array(zId).optional(),
  tag_ids: z.array(zId).optional(),
  tag_filter: z.string().optional(),
  include_hidden_accounts: z.boolean().optional(),
  include_transfers: z.boolean().optional(),
  convert_to: zCurrency.optional(),
  use_transaction_timezone: z.boolean().optional(),
};

type CommonArgs = {
  start?: string;
  end?: string;
  account_ids?: string[];
  category_ids?: string[];
  exclude_category_ids?: string[];
  tag_ids?: string[];
  tag_filter?: string;
  include_hidden_accounts?: boolean;
  include_transfers?: boolean;
  convert_to?: string;
  use_transaction_timezone?: boolean;
};

type Q = Record<string, string | number | boolean | string[] | undefined>;

/** The shared arguments as the plane's query; tag_ids becomes upstream's "has any" filter. */
function commonQuery(a: CommonArgs): Q {
  const tagFilter = a.tag_filter ?? (a.tag_ids !== undefined && a.tag_ids.length > 0 ? `0:${a.tag_ids.join(',')}` : undefined);
  return {
    start: a.start,
    end: a.end,
    account_ids: a.account_ids,
    category_ids: a.category_ids,
    exclude_category_ids: a.exclude_category_ids,
    tag_filter: tagFilter,
    include_hidden_accounts: a.include_hidden_accounts,
    include_transfers: a.include_transfers,
    convert_to: a.convert_to,
    use_transaction_timezone: a.use_transaction_timezone,
  };
}

const CONVERT_INSTEAD = 'Across currencies pass convert_to rather than adding figures yourself; the rates come back named.';

const INTERVALS_ALL = ['none', 'day', 'week', 'month', 'quarter', 'year'] as const;
const INTERVALS_COARSE = ['none', 'month', 'quarter', 'year'] as const;
const INTERVALS_TREND = ['month', 'quarter', 'year'] as const;
const INTERVALS_HISTORY = ['day', 'week', 'month'] as const;
const GROUP_BY = ['primary', 'secondary', 'account'] as const;
const PERIODS = ['today', 'yesterday', 'this_week', 'last_week', 'this_month', 'last_month', 'this_year', 'last_year', 'custom'] as const;

function analytics(name: string, path: string, what: string, insteadOf: string, extraProps: Record<string, unknown>, extraShape: z.ZodRawShape, extraQuery: (a: Record<string, unknown>) => Q): ToolDef {
  return {
    name,
    route: { method: 'GET', path },
    tier: 'read',
    description: describe({ what, tier: 'read', insteadOf }),
    inputSchema: objectSchema({ ...COMMON_PROPERTIES, ...extraProps }),
    schema: z.object({ ...zCommon, ...extraShape }).strict(),
    async run(args, ctx) {
      const a = args as CommonArgs & Record<string, unknown>;
      const res = await ctx.client.request(path, { query: { ...commonQuery(a), ...extraQuery(a) } });
      return fromPlane(res, ANALYTICS_UNTRUSTED);
    },
  };
}

export const periodSummary = analytics(
  'ezb_period_summary',
  '/analytics/period-summary',
  'Gives income and expense for named periods — today, this week, this month, this year, last month — per currency, the way the Overview page does; opening balances are always excluded and transfers unless asked.',
  CONVERT_INSTEAD,
  { periods: { type: 'array', items: { type: 'string', enum: [...PERIODS] }, description: 'Which periods to report. Defaults to today, this_week, this_month, this_year. custom uses start and end.' } },
  { periods: z.array(z.enum(PERIODS)).max(40).optional() },
  a => ({ periods: a.periods as string[] | undefined }),
);

const BY_CATEGORY_PROPS = {
  interval: enumField('Bucket the range into intervals. Defaults to none (one bucket).', INTERVALS_COARSE),
  group_by: enumField('Group by primary category (the default), secondary category, or account.', GROUP_BY),
  top_n: intField('Keep the top N groups and fold the rest into an "other" bucket. 0 (the default) keeps them all.', { minimum: 0, maximum: 1000 }),
  include_zero: boolField('List categories with no transactions as explicit zeros with count 0. Defaults to false: absent is absent, not zero.'),
};
const BY_CATEGORY_SHAPE = { interval: z.enum(INTERVALS_COARSE).optional(), group_by: z.enum(GROUP_BY).optional(), top_n: z.number().int().min(0).max(1000).optional(), include_zero: z.boolean().optional() };
const byCategoryQuery = (a: Record<string, unknown>): Q => ({ interval: a.interval as string | undefined, group_by: a.group_by as string | undefined, top_n: a.top_n as number | undefined, include_zero: a.include_zero as boolean | undefined });

export const spendingByCategory = analytics(
  'ezb_spending_by_category',
  '/analytics/spending-by-category',
  'Breaks spending down by category (primary, secondary or account) over a range, per interval, with a share as two integers and an "other" bucket when top_n bounds it — the pie and bar chart, computed by the app.',
  'Use this rather than summing ezb_list_transactions. ' + CONVERT_INSTEAD,
  BY_CATEGORY_PROPS,
  BY_CATEGORY_SHAPE,
  byCategoryQuery,
);

export const incomeByCategory = analytics(
  'ezb_income_by_category',
  '/analytics/income-by-category',
  'Breaks income down by category (primary, secondary or account) over a range, per interval, with an "other" bucket when top_n bounds it — the income side of the same chart.',
  'For spending use ezb_spending_by_category. ' + CONVERT_INSTEAD,
  BY_CATEGORY_PROPS,
  BY_CATEGORY_SHAPE,
  byCategoryQuery,
);

export const incomeVsExpense = analytics(
  'ezb_income_vs_expense',
  '/analytics/income-vs-expense',
  'Gives income, expense and net per interval (day, week, month, quarter or year) over a range, per currency unless converted — "am I spending more than last year" is interval year.',
  CONVERT_INSTEAD,
  { interval: enumField('The bucket size. Defaults to month.', INTERVALS_ALL) },
  { interval: z.enum(INTERVALS_ALL).optional() },
  a => ({ interval: a.interval as string | undefined }),
);

export const cashFlow = analytics(
  'ezb_cash_flow',
  '/analytics/cash-flow',
  'Gives opening balance, money in, money out and closing balance per interval for a set of accounts — the cash-flow view, per currency unless converted.',
  'For assets against liabilities use ezb_net_worth. ' + CONVERT_INSTEAD,
  { interval: enumField('The bucket size. Defaults to month.', INTERVALS_ALL) },
  { interval: z.enum(INTERVALS_ALL).optional() },
  a => ({ interval: a.interval as string | undefined }),
);

export const netWorth = analytics(
  'ezb_net_worth',
  '/analytics/net-worth',
  'Gives assets, liabilities and net worth per interval and each account\'s closing balance — per currency unless convert_to names the currency the operator thinks in, in which case every rate is reported and historical points use today\'s rates.',
  'For one account over time use ezb_account_balance_history. ' + CONVERT_INSTEAD,
  { interval: enumField('The bucket size. Defaults to month.', INTERVALS_ALL) },
  { interval: z.enum(INTERVALS_ALL).optional() },
  a => ({ interval: a.interval as string | undefined }),
);

export const accountBalanceHistory: ToolDef = {
  name: 'ezb_account_balance_history',
  route: { method: 'GET', path: '/accounts/:id/balance-history' },
  tier: 'read',
  description: describe({
    what: "Gives one account's closing balance over time, by day, week or month, in integer hundredths of the account's own currency, computed by the app.",
    tier: 'read',
    insteadOf: 'For a single date use ezb_get_account_balance; for every account together use ezb_net_worth.',
  }),
  inputSchema: objectSchema(
    {
      account_id: idField('The account id (this route takes ids only; find it with ezb_list_accounts).'),
      start: dateField('First day, inclusive. Defaults to twelve months ago.'),
      end: dateField('Last day, inclusive. Defaults to today.'),
      interval: enumField('The bucket size. Defaults to month; day is capped at 5000 points.', INTERVALS_HISTORY),
    },
    ['account_id'],
  ),
  schema: z.object({ account_id: zId, start: zDate.optional(), end: zDate.optional(), interval: z.enum(INTERVALS_HISTORY).optional() }).strict(),
  async run(args, ctx) {
    const a = args as { account_id: string; start?: string; end?: string; interval?: string };
    const res = await ctx.client.request(`/accounts/${seg(a.account_id)}/balance-history`, { query: { start: a.start, end: a.end, interval: a.interval } });
    return fromPlane(res, ANALYTICS_UNTRUSTED);
  },
};

export const categoryTrend = analytics(
  'ezb_category_trend',
  '/analytics/category-trend',
  'Follows one category (with its sub-categories) over time by month, quarter or year, with its own mean and median — "how much do I really spend on groceries" is this, not one month.',
  'For every category at once use ezb_spending_by_category. ' + CONVERT_INSTEAD,
  {
    category_id: idField('The category to follow (a primary includes its secondaries).'),
    interval: enumField('The bucket size. Defaults to month.', INTERVALS_TREND),
  },
  { category_id: zId, interval: z.enum(INTERVALS_TREND).optional() },
  a => ({ category_id: a.category_id as string, interval: a.interval as string | undefined }),
);
categoryTrend.inputSchema = { ...categoryTrend.inputSchema, required: ['category_id'] };

export const tagBreakdown = analytics(
  'ezb_tag_breakdown',
  '/analytics/tag-breakdown',
  'Gives spending and income per tag over a range — what the trip or the project cost — per currency unless converted.',
  'Find the tag with ezb_list_tags first. ' + CONVERT_INSTEAD,
  { group_by: enumField('Break each tag down by kind (income vs expense, the default), primary category, secondary category or account.', ['kind', ...GROUP_BY]) },
  { tag_ids: z.array(zId).min(1), group_by: z.enum(['kind', ...GROUP_BY]).optional() },
  a => ({ tag_ids: a.tag_ids as string[] | undefined, tag_filter: undefined, group_by: a.group_by as string | undefined }),
);
tagBreakdown.inputSchema = { ...tagBreakdown.inputSchema, required: ['tag_ids'] };

export const payeeLeaderboard = analytics(
  'ezb_payee_leaderboard',
  '/analytics/payee-leaderboard',
  'Ranks who got the money (or who paid it) grouped by normalised transaction comment — ezBookkeeping has no payee entity, so the comment is the merchant — with the raw variants it merged shown as evidence.',
  'For the rows behind one merchant use ezb_list_transactions with keyword. ' + CONVERT_INSTEAD,
  {
    top_n: intField('How many rows. Defaults to 20.', { minimum: 0, maximum: 1000 }),
    direction: enumField('out ranks where money went (the default); in ranks where it came from.', ['out', 'in']),
  },
  { top_n: z.number().int().min(0).max(1000).optional(), direction: z.enum(['out', 'in']).optional() },
  a => ({ top_n: a.top_n as number | undefined, direction: a.direction as string | undefined }),
);

export const listRecurring = analytics(
  'ezb_list_recurring',
  '/analytics/recurring',
  'Detects repeating charges (and income) the operator has not scheduled — subscriptions — with their cadence, every occurrence as evidence and an annualised cost; a detector, not an oracle, so present its findings as findings.',
  'For what IS scheduled use ezb_list_templates with kind scheduled.',
  {
    min_occurrences: intField('How many occurrences before a charge counts as recurring. Defaults to 3.', { minimum: 2, maximum: 1000 }),
    tolerance_days: intField('How far from the expected cadence an occurrence may land. Defaults to 3.', { minimum: 0, maximum: 30 }),
    direction: enumField('out for charges (the default), in for income, both.', ['out', 'in', 'both']),
    include_scheduled: boolField('Also report charges that already have a schedule. Defaults to false.'),
    include_inactive: boolField('Also report series that appear to have stopped. Defaults to false.'),
  },
  {
    min_occurrences: z.number().int().min(2).max(1000).optional(),
    tolerance_days: z.number().int().min(0).max(30).optional(),
    direction: z.enum(['out', 'in', 'both']).optional(),
    include_scheduled: z.boolean().optional(),
    include_inactive: z.boolean().optional(),
  },
  a => ({
    min_occurrences: a.min_occurrences as number | undefined,
    tolerance_days: a.tolerance_days as number | undefined,
    direction: a.direction as string | undefined,
    include_scheduled: a.include_scheduled as boolean | undefined,
    include_inactive: a.include_inactive as boolean | undefined,
  }),
);

export const listAnomalies = analytics(
  'ezb_list_anomalies',
  '/analytics/anomalies',
  'Finds categories unusually far from their own trailing monthly norm, with the trailing mean, the deviation and the rows as evidence; a detector, not an oracle.',
  'For what an import put in a catch-all category use ezb_list_import_fallout.',
  {
    z: strField('How many standard deviations from the trailing mean counts as unusual, as a decimal string. Defaults to "2.0".'),
    min_amount: amountField('Ignore deviations smaller than this.'),
    lookback_months: intField('How many trailing months form the norm. Defaults to 12.', { minimum: 2, maximum: 120 }),
    min_history: intField('How many months of history a category needs before it can be judged. Defaults to 3.', { minimum: 2, maximum: 120 }),
    group_by: enumField('Judge secondary categories (the default) or primary ones.', ['primary', 'secondary']),
  },
  {
    z: z.string().regex(/^\d+(\.\d+)?$/, 'z is a decimal string such as "2.0"').optional(),
    min_amount: hundredths().optional(),
    lookback_months: z.number().int().min(2).max(120).optional(),
    min_history: z.number().int().min(2).max(120).optional(),
    group_by: z.enum(['primary', 'secondary']).optional(),
  },
  a => ({
    z: a.z as string | undefined,
    min_amount: a.min_amount as number | undefined,
    lookback_months: a.lookback_months as number | undefined,
    min_history: a.min_history as number | undefined,
    group_by: a.group_by as string | undefined,
  }),
);

export const getRunway = analytics(
  'ezb_get_runway',
  '/analytics/runway',
  'Gives the runway: liquid assets divided by the trailing average monthly net outflow, in months, with the basis and the assumptions named — say them out loud when you report it.',
  'Pass convert_to when the liquid accounts span currencies; the app names the rates.',
  {
    basis: intField('How many trailing months form the average outflow. Defaults to 6; 3, 6 and 12 are the usual choices.', { minimum: 1, maximum: 24 }),
    liquid_categories: { type: 'array', items: { type: 'string' }, description: 'Which account categories count as liquid. Defaults to the app\'s own choice (cash, checking, savings), echoed in the response.' },
    as_of: dateField('The day the runway is measured from. Defaults to today.'),
  },
  { basis: z.number().int().min(1).max(24).optional(), liquid_categories: z.array(z.string().min(1)).optional(), as_of: zDate.optional() },
  a => ({ basis: a.basis as number | undefined, liquid_categories: a.liquid_categories as string[] | undefined, as_of: a.as_of as string | undefined }),
);

export const listImportFallout: ToolDef = {
  name: 'ezb_list_import_fallout',
  route: { method: 'GET', path: '/analytics/import-fallout' },
  tier: 'read',
  description: describe({
    what: 'Lists the rows an import sent to its fallback category, by account and month and grouped by comment, ready to be re-categorised deliberately.',
    tier: 'read',
    insteadOf: 'To move them, use ezb_set_transaction_category as a preview first.',
  }),
  inputSchema: objectSchema({
    ...COMMON_PROPERTIES,
    run_id: strField('Only rows from this ingest run.'),
    fallback_category_ids: idListField('Which categories count as the fallback. Defaults to the ones the runs recorded.'),
    limit: intField('Rows to return; clamped to this server\'s cap.', { minimum: 1 }),
  }),
  schema: z.object({ ...zCommon, run_id: z.string().min(1).optional(), fallback_category_ids: z.array(zId).optional(), limit: z.number().int().positive().optional() }).strict(),
  async run(args, ctx) {
    const a = args as CommonArgs & { run_id?: string; fallback_category_ids?: string[]; limit?: number };
    const { limit, clamped } = clampLimit(a.limit, ctx.config);
    const res = await ctx.client.request('/analytics/import-fallout', { query: { ...commonQuery(a), run_id: a.run_id, fallback_category_ids: a.fallback_category_ids, limit } });
    const out: ToolResult = fromPlane(res, ANALYTICS_UNTRUSTED);
    if (clamped) {
      out.truncated = true;
      out.limitApplied = limit;
    }
    return out;
  },
};

export const ANALYTICS_TOOLS: ToolDef[] = [
  periodSummary,
  spendingByCategory,
  incomeByCategory,
  incomeVsExpense,
  cashFlow,
  netWorth,
  accountBalanceHistory,
  categoryTrend,
  tagBreakdown,
  payeeLeaderboard,
  listRecurring,
  listAnomalies,
  getRunway,
];
