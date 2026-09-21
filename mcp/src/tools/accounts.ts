/**
 * Accounts — pm/mcp.mdx §9.5. Five read tools.
 */
import { z } from 'zod';

import { boolField, currencyField, dateField, describe, enumField, fromPlane, idField, objectSchema, seg, strField, toolFail, zCurrency, zDate, zId } from './tool.js';
import type { ToolDef } from './tool.js';

export const ACCOUNT_CATEGORIES = ['cash', 'checking', 'savings', 'credit_card', 'virtual', 'debt', 'receivables', 'investment', 'certificate_of_deposit'] as const;

const ACCOUNT_UNTRUSTED = ['name', 'comment'];

/** `account_id` or `account_name`, one of them, resolved by the plane (ids are the contract, names a convenience). */
export const ACCOUNT_REF_PROPERTIES = {
  account_id: idField('The account id.'),
  account_name: strField('The account name instead of its id; exact match first, then case-insensitive. Ambiguity is an error naming the candidates.'),
};

export const zAccountRef = { account_id: zId.optional(), account_name: z.string().min(1).optional() };

export function accountSegment(args: { account_id?: string; account_name?: string }): string {
  if (args.account_id !== undefined) {
    return seg(args.account_id);
  }
  if (args.account_name !== undefined) {
    return seg(args.account_name);
  }
  return toolFail('invalid_input', 'pass account_id or account_name', 'ezb_list_accounts lists the ids and names');
}

export const listAccounts: ToolDef = {
  name: 'ezb_list_accounts',
  route: { method: 'GET', path: '/accounts' },
  tier: 'read',
  description: describe({
    what: "Lists the operator's accounts with their balances in integer hundredths of each account's own currency, sub-accounts nested under their parent (whose balance is null); hidden accounts are excluded unless include_hidden.",
    tier: 'read',
    insteadOf: "For one account's balance as of a past date, use ezb_get_account_balance; for net worth across currencies, use ezb_net_worth with convert_to rather than adding balances yourself.",
  }),
  inputSchema: objectSchema({
    include_hidden: boolField('Include hidden accounts. Defaults to false.'),
    category: enumField('Only accounts of this category.', ACCOUNT_CATEGORIES),
    currency: currencyField('Only accounts in this currency.'),
    with_sub_accounts: boolField('Nest sub-accounts under their parent. Defaults to true.'),
    name: strField('Only the account with this name (exact match first, then case-insensitive), returned as flat rows.'),
  }),
  schema: z
    .object({
      include_hidden: z.boolean().optional(),
      category: z.enum(ACCOUNT_CATEGORIES).optional(),
      currency: zCurrency.optional(),
      with_sub_accounts: z.boolean().optional(),
      name: z.string().min(1).optional(),
    })
    .strict(),
  async run(args, ctx) {
    const a = args as { include_hidden?: boolean; category?: string; currency?: string; with_sub_accounts?: boolean; name?: string };
    const res = await ctx.client.request('/accounts', {
      query: { include_hidden: a.include_hidden, category: a.category, currency: a.currency, with_sub_accounts: a.with_sub_accounts, name: a.name },
    });
    return fromPlane(res, ACCOUNT_UNTRUSTED);
  },
};

export const getAccount: ToolDef = {
  name: 'ezb_get_account',
  route: { method: 'GET', path: '/accounts/:id' },
  tier: 'read',
  description: describe({
    what: 'Gets one account by id or name: its category, asset-or-liability side, currency, balance in integer hundredths, hidden state, and its sub-accounts or parent.',
    tier: 'read',
    insteadOf: 'To find an id, use ezb_list_accounts.',
  }),
  inputSchema: objectSchema(ACCOUNT_REF_PROPERTIES),
  schema: z.object(zAccountRef).strict(),
  async run(args, ctx) {
    const res = await ctx.client.request(`/accounts/${accountSegment(args as { account_id?: string; account_name?: string })}`);
    return fromPlane(res, ACCOUNT_UNTRUSTED);
  },
};

export const getAccountBalance: ToolDef = {
  name: 'ezb_get_account_balance',
  route: { method: 'GET', path: '/accounts/:id/balance' },
  tier: 'read',
  description: describe({
    what: "Gets one account's balance in integer hundredths of its currency as of a date (default today), computed by the app from its daily asset trend; the answer carries the date it was computed for.",
    tier: 'read',
    insteadOf: 'For the balance over time, use ezb_account_balance_history; for all accounts at once, ezb_list_accounts.',
  }),
  inputSchema: objectSchema(
    {
      account_id: idField('The account id (this route takes ids only; find it with ezb_list_accounts).'),
      as_of: dateField('The date the balance is computed for. Defaults to today.'),
    },
    ['account_id'],
  ),
  schema: z.object({ account_id: zId, as_of: zDate.optional() }).strict(),
  async run(args, ctx) {
    const a = args as { account_id: string; as_of?: string };
    const res = await ctx.client.request(`/accounts/${seg(a.account_id)}/balance`, { query: { as_of: a.as_of } });
    return fromPlane(res, ACCOUNT_UNTRUSTED);
  },
};

export const getAccountProperties: ToolDef = {
  name: 'ezb_get_account_properties',
  route: { method: 'GET', path: '/accounts/:id/properties' },
  tier: 'read',
  description: describe({
    what: "Gets one account's transaction count, first and last transaction, last reconciled time, credit-card statement date and limit, and its opening-balance rows — the app's own figures, useful after an import to see what landed.",
    tier: 'read',
    insteadOf: 'For the rows themselves, use ezb_list_transactions with account_ids.',
  }),
  inputSchema: objectSchema(ACCOUNT_REF_PROPERTIES),
  schema: z.object(zAccountRef).strict(),
  async run(args, ctx) {
    const res = await ctx.client.request(`/accounts/${accountSegment(args as { account_id?: string; account_name?: string })}/properties`);
    return fromPlane(res, ACCOUNT_UNTRUSTED);
  },
};

export const getReconciliationStatement: ToolDef = {
  name: 'ezb_get_reconciliation_statement',
  route: { method: 'GET', path: '/accounts/:id/reconciliation' },
  tier: 'read',
  description: describe({
    what: "Gets the app's reconciliation statement for one account over a date range: opening balance, inflows, outflows, closing balance and a running balance per row, all integer hundredths in the account's currency.",
    tier: 'read',
    insteadOf: "To compare against a printed statement's figure and see the difference, use ezb_plan_reconcile.",
  }),
  inputSchema: objectSchema({
    ...ACCOUNT_REF_PROPERTIES,
    start: dateField('First day of the statement, inclusive.'),
    end: dateField('Last day of the statement, inclusive.'),
  }),
  schema: z.object({ ...zAccountRef, start: zDate.optional(), end: zDate.optional() }).strict(),
  async run(args, ctx) {
    const a = args as { account_id?: string; account_name?: string; start?: string; end?: string };
    const res = await ctx.client.request(`/accounts/${accountSegment(a)}/reconciliation`, { query: { start: a.start, end: a.end } });
    return fromPlane(res, ['comment', 'name']);
  },
};

export const ACCOUNT_TOOLS: ToolDef[] = [listAccounts, getAccount, getAccountBalance, getAccountProperties, getReconciliationStatement];
