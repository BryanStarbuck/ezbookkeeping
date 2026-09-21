/**
 * Transactions — pm/mcp.mdx §9.5. Four read tools sharing ONE filter language with the bulk writes
 * (apis.mdx §10.3), so "show me these" and "change these" cannot select different sets.
 */
import { z } from 'zod';

import { amountField, boolField, clampLimit, currencyField, dateField, describe, enumField, fromPlane, hundredths, idField, idListField, intField, objectSchema, seg, strField, zCurrency, zDate, zId } from './tool.js';
import type { ToolDef, ToolResult } from './tool.js';

export const TRANSACTION_TYPES = ['income', 'expense', 'transfer', 'balance_modification'] as const;

export const TRANSACTION_UNTRUSTED = ['comment', 'name'];

/** The filter language (GET /transactions and filter{} on every bulk write). */
export const FILTER_PROPERTIES: Record<string, unknown> = {
  account_ids: idListField('Only these accounts.'),
  account_names: { type: 'array', items: { type: 'string' }, description: 'Only these accounts, by name.' },
  start: dateField('First day, inclusive.'),
  end: dateField('Last day, inclusive.'),
  type: enumField('Only this transaction type.', TRANSACTION_TYPES),
  category_ids: idListField('Only these categories (a primary category includes its secondaries).'),
  category_names: { type: 'array', items: { type: 'string' }, description: 'Only these categories, by name.' },
  tag_ids: idListField('Only transactions carrying any of these tags.'),
  tag_names: { type: 'array', items: { type: 'string' }, description: 'Only transactions carrying any of these tags, by name.' },
  tag_filter: strField("Upstream's tag filter syntax for anything tag_ids cannot say: `<mode>:<tagId>,<tagId>` joined by `;`, mode 0 has any, 1 has all, 2 not has any, 3 not has all."),
  untagged: boolField('Only transactions with no tag at all.'),
  keyword: strField("Only transactions whose comment contains this text (an importer puts the bank's description in the comment)."),
  case_sensitive: boolField('Match keyword case-sensitively. Defaults to false.'),
  min_amount: amountField('Only amounts at or above this, in the transaction\'s own currency; across mixed currencies pass currency too.'),
  max_amount: amountField('Only amounts at or below this, in the transaction\'s own currency.'),
  currency: currencyField('Only transactions in this currency; required with min_amount or max_amount when accounts span currencies.'),
  with_pictures: boolField('Only transactions that have a picture attached (metadata only; pictures are never returned).'),
};

export const zFilter = {
  account_ids: z.array(zId).optional(),
  account_names: z.array(z.string().min(1)).optional(),
  start: zDate.optional(),
  end: zDate.optional(),
  type: z.enum(TRANSACTION_TYPES).optional(),
  category_ids: z.array(zId).optional(),
  category_names: z.array(z.string().min(1)).optional(),
  tag_ids: z.array(zId).optional(),
  tag_names: z.array(z.string().min(1)).optional(),
  tag_filter: z.string().optional(),
  untagged: z.boolean().optional(),
  keyword: z.string().optional(),
  case_sensitive: z.boolean().optional(),
  min_amount: hundredths().optional(),
  max_amount: hundredths().optional(),
  currency: zCurrency.optional(),
  with_pictures: z.boolean().optional(),
};

export type FilterArgs = {
  account_ids?: string[];
  account_names?: string[];
  start?: string;
  end?: string;
  type?: string;
  category_ids?: string[];
  category_names?: string[];
  tag_ids?: string[];
  tag_names?: string[];
  tag_filter?: string;
  untagged?: boolean;
  keyword?: string;
  case_sensitive?: boolean;
  min_amount?: number;
  max_amount?: number;
  currency?: string;
  with_pictures?: boolean;
};

const FILTER_KEYS = Object.keys(FILTER_PROPERTIES);

/** The filter as a GET query (lists comma-joined; the plane splits them). */
export function filterQuery(a: FilterArgs): Record<string, string | number | boolean | string[] | undefined> {
  const q: Record<string, string | number | boolean | string[] | undefined> = {};
  for (const k of FILTER_KEYS) {
    const v = (a as Record<string, unknown>)[k];
    if (v !== undefined) {
      q[k] = v as string | number | boolean | string[];
    }
  }
  return q;
}

/** Just the filter keys of an argument object (for filter{} on a bulk write). */
export function filterOnly(a: Record<string, unknown>): FilterArgs {
  const out: Record<string, unknown> = {};
  for (const k of FILTER_KEYS) {
    if (a[k] !== undefined) {
      out[k] = a[k];
    }
  }
  return out;
}

export const listTransactions: ToolDef = {
  name: 'ezb_list_transactions',
  route: { method: 'GET', path: '/transactions' },
  tier: 'read',
  description: describe({
    what: "Lists transactions matching the filters, newest first, each with its type, integer-hundredths amount and currency, category, tags and comment; a transfer appears once with both sides, and a page over the cap returns truncated with a nextCursor.",
    tier: 'read',
    insteadOf: 'Never add these up: for a total use ezb_spending_by_category or another analytics tool, and for how many match use ezb_count_transactions rather than the length of a page.',
  }),
  inputSchema: objectSchema({
    ...FILTER_PROPERTIES,
    limit: intField('Rows to return; clamped to this server\'s cap, which is reported as limitApplied when it bound.', { minimum: 1 }),
    cursor: strField('The nextCursor a previous page returned, to fetch the next page.'),
    order: enumField('Sort order: -time is newest first (the default), time is oldest first (cannot be combined with cursor).', ['-time', 'time']),
  }),
  schema: z
    .object({
      ...zFilter,
      limit: z.number().int().positive().optional(),
      cursor: z.string().regex(/^\d+$/).optional(),
      order: z.enum(['-time', 'time']).optional(),
    })
    .strict(),
  async run(args, ctx) {
    const a = args as FilterArgs & { limit?: number; cursor?: string; order?: string };
    const { limit, clamped } = clampLimit(a.limit, ctx.config);
    const res = await ctx.client.request('/transactions', { query: { ...filterQuery(a), limit, cursor: a.cursor, order: a.order } });
    const out: ToolResult = fromPlane(res, TRANSACTION_UNTRUSTED);
    if (clamped) {
      out.truncated = true;
      out.limitApplied = limit;
    }
    return out;
  },
};

export const getTransaction: ToolDef = {
  name: 'ezb_get_transaction',
  route: { method: 'GET', path: '/transactions/:id' },
  tier: 'read',
  description: describe({
    what: "Gets one transaction by id with its tags, whether it has pictures (metadata only), and both sides of a transfer with each side's amount in its own currency.",
    tier: 'read',
    insteadOf: 'To find an id, use ezb_list_transactions with a keyword or a date range.',
  }),
  inputSchema: objectSchema({ transaction_id: idField('The transaction id.') }, ['transaction_id']),
  schema: z.object({ transaction_id: zId }).strict(),
  async run(args, ctx) {
    const { transaction_id } = args as { transaction_id: string };
    const res = await ctx.client.request(`/transactions/${seg(transaction_id)}`);
    return fromPlane(res, TRANSACTION_UNTRUSTED);
  },
};

export const countTransactions: ToolDef = {
  name: 'ezb_count_transactions',
  route: { method: 'GET', path: '/transactions/count' },
  tier: 'read',
  description: describe({
    what: "Counts the transactions matching the same filters as ezb_list_transactions — the app's own count, where a transfer counts once.",
    tier: 'read',
    insteadOf: 'Use this rather than the length of a page from ezb_list_transactions, which is capped.',
  }),
  inputSchema: objectSchema(FILTER_PROPERTIES),
  schema: z.object(zFilter).strict(),
  async run(args, ctx) {
    const res = await ctx.client.request('/transactions/count', { query: filterQuery(args as FilterArgs) });
    return fromPlane(res);
  },
};

export const exportTransactions: ToolDef = {
  name: 'ezb_export_transactions',
  route: { method: 'GET', path: '/transactions/export' },
  tier: 'read',
  description: describe({
    what: "Exports the transactions matching the same filters as ezb_list_transactions as the app's own CSV or TSV text, for a spreadsheet; the file content is returned in data.content.",
    tier: 'read',
    insteadOf: 'For rows to reason about, use ezb_list_transactions; for totals, the analytics tools.',
  }),
  inputSchema: objectSchema({
    ...FILTER_PROPERTIES,
    format: enumField('The file format. Defaults to csv.', ['csv', 'tsv']),
  }),
  schema: z.object({ ...zFilter, format: z.enum(['csv', 'tsv']).optional() }).strict(),
  async run(args, ctx) {
    const a = args as FilterArgs & { format?: string };
    const res = await ctx.client.request('/transactions/export', { query: { ...filterQuery(a), format: a.format ?? 'csv' } });
    return fromPlane(res, ['content']);
  },
};

export const TRANSACTION_TOOLS: ToolDef[] = [listTransactions, getTransaction, countTransactions, exportTransactions];
