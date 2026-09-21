/**
 * Own-account moves as transfers, and the one sanctioned removal — pm/mcp.mdx §9.5b.
 *
 * A statement import writes a move between two of the operator's own accounts twice: an expense in
 * one account and an income in the other, both counted as spending and earning. These tools find
 * such pairs (ezb_find_transfer_pairs, reads only) and turn them into ONE transfer, or turn a single
 * row into a transfer against a named counterpart account (ezb_convert_to_transfer, previewed,
 * confirmed, undoable). ezb_delete_transactions is the admin-tier removal the operator asked for:
 * ids only, previewed, confirmed, undoable, and behind a third switch.
 *
 * Every tool makes ONE machine-plane call; the pairing, the checks and the balance assertion are the
 * plane's (apis.mdx §10.3.2).
 */
import { z } from 'zod';

import { boolField, dateField, describe, enumField, fromPlane, idField, idListField, intField, objectSchema, strField, WRITE_PROPERTIES, withoutWriteKeys, writeOpts, zDate, zId, zWrite } from './tool.js';
import type { ToolDef, WriteArgs } from './tool.js';

const UNTRUSTED = ['comment', 'accountName', 'sourceAccountName', 'destinationAccountName', 'categoryName'];

export const findTransferPairs: ToolDef = {
  name: 'ezb_find_transfer_pairs',
  route: { method: 'POST', path: '/transactions/transfer-candidates' },
  tier: 'read',
  description: describe({
    what: "Finds moves between the operator's own accounts that a statement import recorded twice — an expense in one account and an income of the same amount and currency in another, at most window_days apart, with a hint (a comment carrying the other account's last four digits, or a word such as PAYMENT, AUTOPAY, TRANSFER, XFER, THANK YOU) — and returns the unambiguous pairs, each with a ready `item` for ezb_convert_to_transfer, apart from ambiguous groups where one row has several candidates.",
    tier: 'read',
    insteadOf: 'Show the operator the pairs, then pass the chosen `item`s to ezb_convert_to_transfer. Never convert an ambiguous group without the operator choosing each pair.',
  }),
  inputSchema: objectSchema({
    account_ids: idListField('Only these accounts (a parent includes its sub-accounts). Defaults to every visible account.'),
    account_names: { type: 'array', items: { type: 'string' }, description: 'Only these accounts, by name.' },
    start: dateField('First day, inclusive (a counterpart up to window_days earlier still counts).'),
    end: dateField('Last day, inclusive (a counterpart up to window_days later still counts).'),
    window_days: intField('How many days apart the two sides may post. Defaults to 4; at most 31.', { minimum: 0, maximum: 31 }),
    require_hint: boolField('Only candidates with a last-four or transfer-word hint (the default, true). false also returns same-amount coincidences — review those row by row.'),
    limit: intField('The most pairs (and, separately, ambiguous groups) returned. Defaults to 1000. The counts always cover everything found.', { minimum: 1 }),
  }),
  schema: z
    .object({
      account_ids: z.array(zId).optional(),
      account_names: z.array(z.string().min(1)).optional(),
      start: zDate.optional(),
      end: zDate.optional(),
      window_days: z.number().int().min(0).max(31).optional(),
      require_hint: z.boolean().optional(),
      limit: z.number().int().positive().optional(),
    })
    .strict(),
  async run(args, ctx) {
    const res = await ctx.client.request('/transactions/transfer-candidates', { method: 'POST', body: args as Record<string, unknown> });
    return fromPlane(res, UNTRUSTED);
  },
};

const ITEM_SCHEMA = {
  type: 'object',
  properties: {
    id: idField('The row to convert (for a pair, either side).'),
    counter_id: idField('Pair: the other row — one of the two is an expense and the other an income, in different accounts, of the same amount.'),
    counter_account_id: idField('Single row: the account on the other side of the move (typically a hidden counterpart account). Same currency as the row.'),
    counter_account_name: strField('Single row: the counterpart account by name instead of id.'),
  },
  required: ['id'],
  additionalProperties: false,
};

const zItem = z
  .object({ id: zId, counter_id: zId.optional(), counter_account_id: zId.optional(), counter_account_name: z.string().min(1).optional() })
  .strict();

export const convertToTransfer: ToolDef = {
  name: 'ezb_convert_to_transfer',
  route: { method: 'POST', path: '/transactions/convert-to-transfer' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: "Turns income/expense rows into transfers so a move between the operator's own accounts stops counting as income and spending: a pair {id, counter_id} becomes ONE transfer from the expense's account to the income's account (the expense's date; comments joined with ⇄; tags merged), and a single row {id, counter_account_id|counter_account_name} becomes a transfer against that account (e.g. an investment account's monthly change in value against a hidden counterpart). Every account balance stays the same (the plan asserts it), the statement import records follow the new transfer so a later re-plan still finds those rows already present, and ezb_undo reverses the whole conversion.",
    tier: 'write',
    insteadOf: 'Find pairs with ezb_find_transfer_pairs first and show the operator the preview (before rows, the transfer, the balance effect per account).',
  }),
  inputSchema: objectSchema(
    {
      items: { type: 'array', items: ITEM_SCHEMA, minItems: 1, description: 'The conversions: each item is a pair (id + counter_id) or a single row (id + counter_account_id or counter_account_name). A transaction may appear only once in a request.' },
      comment_mode: enumField("The pair's comment: both (the default: expense comment ⇄ income comment) or out (the expense comment only).", ['both', 'out']),
      transfer_category_id: idField("The transfer SUB-category the transfers get. Defaults to the operator's first visible transfer sub-category; with none the call fails and says how to add one."),
      transfer_category_name: strField('The transfer sub-category by name ("Group > Sub" disambiguates) instead of id.'),
      ...WRITE_PROPERTIES,
    },
    ['items'],
  ),
  schema: z
    .object({
      items: z.array(zItem).min(1),
      comment_mode: z.enum(['both', 'out']).optional(),
      transfer_category_id: zId.optional(),
      transfer_category_name: z.string().min(1).optional(),
      ...zWrite,
    })
    .strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/transactions/convert-to-transfer', { method: 'POST', body: { ...withoutWriteKeys(a), ...writeOpts(a, ctx.config) } });
    return fromPlane(res, UNTRUSTED);
  },
};

export const deleteTransactions: ToolDef = {
  name: 'ezb_delete_transactions',
  route: { method: 'DELETE', path: '/transactions/bulk' },
  tier: 'write',
  admin: true,
  hasDryRun: true,
  description: describe({
    what: 'Deletes transactions by id (a transfer is one id) — the one removal this server has, at the admin tier: the preview lists every row that would go, the apply needs its confirm_token, ids already gone are reported as missing, and ezb_undo re-creates the deleted rows (with new ids, without pictures).',
    tier: 'write',
    insteadOf: 'Only when the operator has asked for these exact rows to be removed. A duplicate own-account move wants ezb_convert_to_transfer, not a delete. Needs EZBKMCP_ALLOW_ADMIN=1 here and `ezbk up --allow-write --allow-admin` on the app.',
  }),
  inputSchema: objectSchema({ ids: idListField('The transactions to delete. Ids only, never a filter.'), ...WRITE_PROPERTIES }, ['ids']),
  schema: z
    .object({ ids: z.array(zId).min(1), ...zWrite })
    .strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & { ids: string[] };
    const res = await ctx.client.request('/transactions/bulk', { method: 'DELETE', body: { ids: a.ids, ...writeOpts(a, ctx.config) } });
    return fromPlane(res, UNTRUSTED);
  },
};

export const TRANSFER_TOOLS: ToolDef[] = [findTransferPairs, convertToTransfer, deleteTransactions];
