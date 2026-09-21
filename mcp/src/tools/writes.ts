/**
 * The write tier — pm/mcp.mdx §9.5, §9.7. Twenty-one tools, off by default. Every one but ezb_undo
 * previews by default (dry_run: true) and applies only with dry_run: false plus the confirm_token
 * the preview returned, under a max_changes ceiling; the plane recomputes the change set at apply
 * time and refuses a moved fingerprint with conflict. None deletes anything.
 */
import { z } from 'zod';

import { ACCOUNT_CATEGORIES, ACCOUNT_REF_PROPERTIES, accountSegment, zAccountRef } from './accounts.js';
import { CATEGORY_TYPES } from './reference.js';
import { PLAN_FILE_PROPERTIES, PLAN_IMPORT_PROPERTIES, withRoot, zPlanFile, zPlanImport } from './statements.js';
import { amountField, boolField, currencyField, dateField, describe, enumField, fromPlane, hundredths, idField, idListField, intField, objectSchema, seg, strField, toolFail, WRITE_PROPERTIES, withoutWriteKeys, writeOpts, zCurrency, zDate, zId, zWrite } from './tool.js';
import type { ToolDef, ToolResult, WriteArgs } from './tool.js';
import { FILTER_PROPERTIES, filterOnly, zFilter } from './transactions.js';

const WRITE_UNTRUSTED = ['comment', 'name'];

/** Every write's data carries the preview (dry run) or the result, and the plane's meta.dryRun. */
function fromWrite(res: Parameters<typeof fromPlane>[0]): ToolResult {
  return fromPlane(res, WRITE_UNTRUSTED);
}

// ─── statements: the three applies ───────────────────────────────────────────────────────────

const RECALL = 'With only the confirm_token, the plane recalls the plan\'s own arguments; pass them again to be explicit.';

export const applyAccounts: ToolDef = {
  name: 'ezb_apply_accounts',
  route: { method: 'POST', path: '/ingest/accounts/apply' },
  tier: 'write',
  noTimeout: true,
  hasDryRun: true,
  description: describe({
    what: 'Creates the accounts a statement archive needs — the plan\'s create rows in one batch, the link rows linked — and writes the statements-to-books map beside the statements; ambiguous rows are never created, and the whole batch is undoable.',
    tier: 'write',
    insteadOf: 'Call ezb_plan_accounts first; its confirm_token is what this needs. ' + RECALL,
  }),
  inputSchema: objectSchema({
    root: strField('The statements root. Defaults to the configured one.'),
    manifest_path: strField('The manifest file, relative to the root.'),
    staging: strField('The staging directory. Defaults to {root}/.ezbk-staging.'),
    naming: strField('The name template the plan used.'),
    kind_to_category: { type: 'object', additionalProperties: { type: 'string' }, description: 'The kind-to-category overrides the plan used.' },
    overrides: {
      type: 'object',
      additionalProperties: {
        type: 'object',
        properties: { name: { type: 'string' }, action: { type: 'string', enum: ['skip', 'link', 'create'] }, account_id: { type: 'string' } },
        additionalProperties: false,
      },
      description: "The operator's per-account resolutions the plan used.",
    },
    accounts: { type: 'array', items: { type: 'string' }, description: 'Only these manifest accounts.' },
    ...WRITE_PROPERTIES,
  }),
  schema: z
    .object({
      root: z.string().min(1).optional(),
      manifest_path: z.string().min(1).optional(),
      staging: z.string().min(1).optional(),
      naming: z.string().min(1).optional(),
      kind_to_category: z.record(z.string(), z.string()).optional(),
      overrides: z.record(z.string(), z.object({ name: z.string().optional(), action: z.enum(['skip', 'link', 'create']).optional(), account_id: zId.optional() }).strict()).optional(),
      accounts: z.array(z.string().min(1)).optional(),
      ...zWrite,
    })
    .strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/ingest/accounts/apply', { method: 'POST', body: { ...withRoot(withoutWriteKeys(a), ctx), ...writeOpts(a, ctx.config) }, noTimeout: true });
    return fromWrite(res);
  },
};

export const applyStatementImport: ToolDef = {
  name: 'ezb_apply_statement_import',
  route: { method: 'POST', path: '/ingest/apply' },
  tier: 'write',
  hasDryRun: true,
  noTimeout: true,
  description: describe({
    what: "Runs a statement import plan: recomputes the plan, refuses with conflict and the new counts if the books moved since the token, then imports only the NEW rows through the app's own import path, records every row in the plane's import record, and journals it for ezb_undo. Rows the operator deleted in the app stay deleted — there is no argument that re-adds them.",
    tier: 'write',
    insteadOf: 'Call ezb_plan_statement_import first and show the operator its unmapped categories, transfer candidates and conflicts; its confirm_token is what this needs. ' + RECALL,
  }),
  inputSchema: objectSchema({ ...PLAN_IMPORT_PROPERTIES, ...WRITE_PROPERTIES }),
  schema: z.object({ ...zPlanImport, ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/ingest/apply', { method: 'POST', body: { ...withRoot(withoutWriteKeys(a), ctx), ...writeOpts(a, ctx.config) }, noTimeout: true });
    return fromWrite(res);
  },
};

export const applyFileImport: ToolDef = {
  name: 'ezb_apply_file_import',
  route: { method: 'POST', path: '/ingest/file/apply' },
  tier: 'write',
  hasDryRun: true,
  noTimeout: true,
  description: describe({
    what: "Imports one planned file into one account — the Import dialog applied — recording every row in the plane's import record and journaling it for ezb_undo.",
    tier: 'write',
    insteadOf: 'Call ezb_plan_file_import first; its confirm_token is what this needs. ' + RECALL,
  }),
  inputSchema: objectSchema({ ...PLAN_FILE_PROPERTIES, ...WRITE_PROPERTIES }),
  schema: z.object({ ...zPlanFile, ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/ingest/file/apply', { method: 'POST', body: { ...withRoot(withoutWriteKeys(a), ctx), ...writeOpts(a, ctx.config) }, noTimeout: true });
    return fromWrite(res);
  },
};

// ─── transactions ────────────────────────────────────────────────────────────────────────────

const TXN_WRITE_TYPES = ['income', 'expense', 'transfer'] as const;

const TXN_ROW_PROPERTIES: Record<string, unknown> = {
  type: enumField('The transaction type.', TXN_WRITE_TYPES),
  date: dateField('The day of the transaction.'),
  time: strField('The time of day as HH:MM or HH:MM:SS. Defaults to noon.'),
  account_id: idField('The account (for a transfer, the source).'),
  account_name: strField('The account by name instead of id.'),
  amount: amountField("The amount as a positive magnitude in the account's currency; the type carries the direction."),
  destination_account_id: idField('For a transfer: the destination account.'),
  destination_account_name: strField('For a transfer: the destination account by name.'),
  destination_amount: amountField("For a cross-currency transfer: the amount received, in the destination account's currency. Defaults to amount."),
  category_id: idField('The SECONDARY category (a primary is refused, listing its children); its type must match the transaction type.'),
  category_name: strField('The secondary category by name instead of id.'),
  tag_ids: idListField('Tags to attach (at most 10).'),
  tag_names: { type: 'array', items: { type: 'string' }, description: 'Tags to attach, by name.' },
  comment: strField('The comment — the merchant or description, the only free-text field a transaction has.'),
  hide_amount: boolField('Hide the amount in the UI. Defaults to false.'),
  geo: {
    type: 'object',
    properties: { latitude: { type: 'number' }, longitude: { type: 'number' } },
    required: ['latitude', 'longitude'],
    additionalProperties: false,
    description: 'A location, if the operator gave one.',
  },
};

const zTxnRow = z
  .object({
    type: z.enum(TXN_WRITE_TYPES),
    date: zDate,
    time: z.string().regex(/^\d{2}:\d{2}(:\d{2})?$/).optional(),
    account_id: zId.optional(),
    account_name: z.string().min(1).optional(),
    amount: hundredths(),
    destination_account_id: zId.optional(),
    destination_account_name: z.string().min(1).optional(),
    destination_amount: hundredths().optional(),
    category_id: zId.optional(),
    category_name: z.string().min(1).optional(),
    tag_ids: z.array(zId).max(10).optional(),
    tag_names: z.array(z.string().min(1)).max(10).optional(),
    comment: z.string().optional(),
    hide_amount: z.boolean().optional(),
    geo: z.object({ latitude: z.number(), longitude: z.number() }).strict().optional(),
  })
  .strict();

export const addTransactions: ToolDef = {
  name: 'ezb_add_transactions',
  route: { method: 'POST', path: '/transactions' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Adds one or more transactions — income, expense or transfer — each with an integer-hundredths amount in its account\'s currency, a secondary category of the matching type, optional tags and comment; a transfer between two currencies carries both amounts, and the preview shows every row as it will be stored.',
    tier: 'write',
    insteadOf: 'To bring a bank file in, use ezb_plan_file_import instead of typing its rows.',
  }),
  inputSchema: objectSchema(
    {
      transactions: {
        type: 'array',
        minItems: 1,
        items: { type: 'object', properties: TXN_ROW_PROPERTIES, required: ['type', 'date', 'amount'], additionalProperties: false },
        description: 'The transactions to add, in order.',
      },
      idempotency_key: strField('An optional key (up to 128 characters) so a repeated submission returns the original answer instead of adding twice.'),
      ...WRITE_PROPERTIES,
    },
    ['transactions'],
  ),
  schema: z.object({ transactions: z.array(zTxnRow).min(1), idempotency_key: z.string().max(128).optional(), ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & { transactions: unknown[]; idempotency_key?: string };
    const res = await ctx.client.request('/transactions', {
      method: 'POST',
      body: { transactions: a.transactions, ...(a.idempotency_key === undefined ? {} : { idempotency_key: a.idempotency_key }), ...writeOpts(a, ctx.config) },
    });
    return fromWrite(res);
  },
};

export const updateTransaction: ToolDef = {
  name: 'ezb_update_transaction',
  route: { method: 'PATCH', path: '/transactions/:id' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: "Edits one transaction's fields — type, date, time, account, amount, destination, category, tags, comment, geo — with a preview that is the exact field-by-field before and after; absent fields stay as they are.",
    tier: 'write',
    insteadOf: 'To change the category or account of many transactions at once, use ezb_set_transaction_category or ezb_set_transaction_account.',
  }),
  inputSchema: objectSchema(
    {
      transaction_id: idField('The transaction to edit.'),
      ...TXN_ROW_PROPERTIES,
      clear_geo: boolField('Remove the location.'),
      ...WRITE_PROPERTIES,
    },
    ['transaction_id'],
  ),
  schema: z.object({ transaction_id: zId, ...zTxnRow.partial().shape, clear_geo: z.boolean().optional(), ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & { transaction_id: string } & Record<string, unknown>;
    const { transaction_id: _t, ...rest } = withoutWriteKeys(a);
    const res = await ctx.client.request(`/transactions/${seg(a.transaction_id)}`, { method: 'PATCH', body: { ...rest, ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

/** `ids[]` or `filter{}` — the same filter language as ezb_list_transactions, so the set shown is the set changed. */
const BULK_SELECT_PROPERTIES: Record<string, unknown> = {
  ids: idListField('The transactions to change, by id.'),
  filter: {
    type: 'object',
    properties: FILTER_PROPERTIES,
    additionalProperties: false,
    description: 'Or the transactions to change by the same filter ezb_list_transactions takes; the preview names every row and the exact count.',
  },
};
const zBulkSelect = { ids: z.array(zId).min(1).optional(), filter: z.object(zFilter).strict().optional() };
type BulkSelect = { ids?: string[]; filter?: Record<string, unknown> };

function bulkBody(a: BulkSelect, extra: Record<string, unknown>, config: Parameters<typeof writeOpts>[1], w: WriteArgs): Record<string, unknown> {
  if (a.ids === undefined && a.filter === undefined) {
    toolFail('invalid_input', 'pass ids or filter to choose the transactions', 'show them first with ezb_list_transactions using the same filter');
  }
  return {
    ...(a.ids === undefined ? {} : { ids: a.ids }),
    ...(a.filter === undefined ? {} : { filter: filterOnly(a.filter) }),
    ...extra,
    ...writeOpts(w, config),
  };
}

export const setTransactionCategory: ToolDef = {
  name: 'ezb_set_transaction_category',
  route: { method: 'POST', path: '/transactions/set-category' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Re-categorises transactions chosen by ids or by a filter into ONE secondary category, under a ceiling; with allow_type_change an income row may become an expense or back when the category is of the other type; the preview names every row and its before-and-after category and type.',
    tier: 'write',
    insteadOf: 'To give many rows different categories in one write, use ezb_set_transaction_categories. Show the rows first with ezb_list_transactions (the same filter) or ezb_list_uncategorized.',
  }),
  inputSchema: objectSchema({
    ...BULK_SELECT_PROPERTIES,
    category_id: idField('The secondary category to set.'),
    category_name: strField('The secondary category by name or path: "Sub", "Group > Sub" or "Type > Group > Sub" (e.g. "Expense > Food & Drink > Food").'),
    allow_type_change: boolField('Let income rows become expense rows (or back) when the category is of the other type. Defaults to false. Turning a row into a transfer needs ezb_set_transaction_categories with a counter account.'),
    ...WRITE_PROPERTIES,
  }),
  schema: z.object({ ...zBulkSelect, category_id: zId.optional(), category_name: z.string().min(1).optional(), allow_type_change: z.boolean().optional(), ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & BulkSelect & { category_id?: string; category_name?: string; allow_type_change?: boolean };
    const res = await ctx.client.request('/transactions/set-category', {
      method: 'POST',
      body: bulkBody(
        a,
        {
          ...(a.category_id === undefined ? {} : { category_id: a.category_id }),
          ...(a.category_name === undefined ? {} : { category_name: a.category_name }),
          ...(a.allow_type_change === undefined ? {} : { allow_type_change: a.allow_type_change }),
        },
        ctx.config,
        a,
      ),
    });
    return fromWrite(res);
  },
};

const ASSIGNMENT_PROPERTIES: Record<string, unknown> = {
  ids: idListField('The transactions this assignment categorises (a group\'s ids from ezb_list_uncategorized). Each id may appear in one assignment only.'),
  category: strField('The category as a path: "Type > Group > Sub" names the major category (Income, Expense or Transfer), the group and the sub-category exactly; "Group > Sub" or "Sub" picks the one of each row\'s own type.'),
  category_id: idField('The secondary category by id instead of a path.'),
  counter_account_id: idField("Only when a row becomes a transfer: the operator's other account. An expense row pays INTO it; an income row came FROM it."),
  counter_account_name: strField('The counter account by name instead of id.'),
  counter_amount: amountField("The amount on the counter account's side, when it is in another currency."),
};

export const setTransactionCategories: ToolDef = {
  name: 'ezb_set_transaction_categories',
  route: { method: 'POST', path: '/transactions/categorize' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Categorises many transactions in one previewed, undoable write — each assignment gives its own ids a category path "Type > Group > Sub" — so a categoriser can move a whole page of payee groups at once; with allow_type_change a row may change its major category (income, expense or transfer, the last naming the counter account).',
    tier: 'write',
    insteadOf: 'Get the groups and ids from ezb_list_uncategorized; create missing categories first with ezb_add_categories. For one category over a filter, ezb_set_transaction_category.',
  }),
  inputSchema: objectSchema(
    {
      assignments: {
        type: 'array',
        minItems: 1,
        items: { type: 'object', properties: ASSIGNMENT_PROPERTIES, required: ['ids'], additionalProperties: false },
        description: 'One entry per decision: which rows, which category. Up to 5000 rows in one call.',
      },
      allow_type_change: boolField('Let a row move to another major category when its category is of another type. Defaults to false: a mismatch is refused, naming the row.'),
      skip_invalid: boolField('List rows that cannot take their category under preview.skipped and change the rest, instead of refusing the whole call. Defaults to false.'),
      summary_only: boolField('Leave the per-row before-and-after out of the preview and keep the per-category counts (preview.byCategory). Use it for large batches; the confirm_token covers the same rows either way.'),
      ...WRITE_PROPERTIES,
    },
    ['assignments'],
  ),
  schema: z
    .object({
      assignments: z
        .array(
          z
            .object({
              ids: z.array(zId).min(1),
              category: z.string().min(1).optional(),
              category_id: zId.optional(),
              counter_account_id: zId.optional(),
              counter_account_name: z.string().min(1).optional(),
              counter_amount: hundredths().optional(),
            })
            .strict(),
        )
        .min(1)
        .max(5000),
      allow_type_change: z.boolean().optional(),
      skip_invalid: z.boolean().optional(),
      summary_only: z.boolean().optional(),
      ...zWrite,
    })
    .strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/transactions/categorize', { method: 'POST', body: { ...withoutWriteKeys(a), ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

export const setTransactionAccount: ToolDef = {
  name: 'ezb_set_transaction_account',
  route: { method: 'POST', path: '/transactions/set-account' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Moves transactions chosen by ids or by a filter to another account of the same currency ("that was on the card, not checking"), under a ceiling; for a transfer, side says which end moves.',
    tier: 'write',
    insteadOf: 'Show the rows first with ezb_get_transaction or ezb_list_transactions.',
  }),
  inputSchema: objectSchema({
    ...BULK_SELECT_PROPERTIES,
    ...ACCOUNT_REF_PROPERTIES,
    side: enumField('For transfers: which side moves. Defaults to source.', ['source', 'destination']),
    ...WRITE_PROPERTIES,
  }),
  schema: z.object({ ...zBulkSelect, ...zAccountRef, side: z.enum(['source', 'destination']).optional(), ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & BulkSelect & { account_id?: string; account_name?: string; side?: string };
    const res = await ctx.client.request('/transactions/set-account', {
      method: 'POST',
      body: bulkBody(
        a,
        { ...(a.account_id === undefined ? {} : { account_id: a.account_id }), ...(a.account_name === undefined ? {} : { account_name: a.account_name }), ...(a.side === undefined ? {} : { side: a.side }) },
        ctx.config,
        a,
      ),
    });
    return fromWrite(res);
  },
};

const TAG_SELECT_PROPERTIES: Record<string, unknown> = {
  tag_ids: idListField('The tags, by id.'),
  tag_names: { type: 'array', items: { type: 'string' }, description: 'The tags, by name.' },
};
const zTagSelect = { tag_ids: z.array(zId).optional(), tag_names: z.array(z.string().min(1)).optional() };

export const addTransactionTags: ToolDef = {
  name: 'ezb_add_transaction_tags',
  route: { method: 'POST', path: '/transactions/tags/add' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Adds tags to transactions chosen by ids or by a filter ("tag everything from last week in Lisbon as Trip"), under a ceiling; a row that would exceed ten tags is refused by name.',
    tier: 'write',
    insteadOf: 'Show the rows first with ezb_list_transactions; find the tag with ezb_list_tags or add it with ezb_add_tag.',
  }),
  inputSchema: objectSchema({ ...BULK_SELECT_PROPERTIES, ...TAG_SELECT_PROPERTIES, ...WRITE_PROPERTIES }),
  schema: z.object({ ...zBulkSelect, ...zTagSelect, ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & BulkSelect & { tag_ids?: string[]; tag_names?: string[] };
    const res = await ctx.client.request('/transactions/tags/add', {
      method: 'POST',
      body: bulkBody(a, { ...(a.tag_ids === undefined ? {} : { tag_ids: a.tag_ids }), ...(a.tag_names === undefined ? {} : { tag_names: a.tag_names }) }, ctx.config, a),
    });
    return fromWrite(res);
  },
};

export const removeTransactionTags: ToolDef = {
  name: 'ezb_remove_transaction_tags',
  route: { method: 'POST', path: '/transactions/tags/remove' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Removes tags from transactions chosen by ids or by a filter, under a ceiling; the preview names every row it would change.',
    tier: 'write',
    insteadOf: 'Show the rows first with ezb_list_transactions using tag_ids.',
  }),
  inputSchema: objectSchema({ ...BULK_SELECT_PROPERTIES, ...TAG_SELECT_PROPERTIES, ...WRITE_PROPERTIES }),
  schema: z.object({ ...zBulkSelect, ...zTagSelect, ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & BulkSelect & { tag_ids?: string[]; tag_names?: string[] };
    const res = await ctx.client.request('/transactions/tags/remove', {
      method: 'POST',
      body: bulkBody(a, { ...(a.tag_ids === undefined ? {} : { tag_ids: a.tag_ids }), ...(a.tag_names === undefined ? {} : { tag_names: a.tag_names }) }, ctx.config, a),
    });
    return fromWrite(res);
  },
};

// ─── accounts, categories, tags ──────────────────────────────────────────────────────────────

const SUB_ACCOUNT_PROPERTIES: Record<string, unknown> = {
  name: strField('The sub-account name.'),
  currency: currencyField('Its currency (immutable later).'),
  initial_balance: amountField('Its opening balance.'),
  initial_balance_date: dateField('The day the opening balance is as of.'),
  color: strField('A hex RGB colour such as 000000.'),
  icon: strField('An icon id.'),
  comment: strField('A comment.'),
};

export const addAccount: ToolDef = {
  name: 'ezb_add_account',
  route: { method: 'POST', path: '/accounts' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Adds one account — its category decides asset or liability and so the sign of every balance, its currency is immutable after creation, and an initial balance becomes the opening-balance row (negative for money owed on a card or loan); the preview shows all three, so read them out.',
    tier: 'write',
    insteadOf: 'For the accounts a statement archive needs, use ezb_plan_accounts and ezb_apply_accounts instead.',
  }),
  inputSchema: objectSchema(
    {
      name: strField('The account name (up to 64 characters).'),
      category: enumField('The account category.', ACCOUNT_CATEGORIES),
      currency: currencyField('The currency — immutable after creation.'),
      initial_balance: amountField('The opening balance, stored as a balance-modification row dated initial_balance_date. Negative for a liability owed.'),
      initial_balance_date: dateField('The day the opening balance is as of. Defaults to today.'),
      color: strField('A hex RGB colour such as 000000.'),
      icon: strField('An icon id.'),
      comment: strField('A comment.'),
      credit_card_statement_date: intField('For a credit card: the day of the month the statement closes.', { minimum: 0, maximum: 28 }),
      credit_card_limit: amountField('For a credit card: the credit limit.'),
      sub_accounts: { type: 'array', items: { type: 'object', properties: SUB_ACCOUNT_PROPERTIES, required: ['name', 'currency'], additionalProperties: false }, description: 'Sub-accounts, if this is a parent account (a parent holds no balance of its own).' },
      idempotency_key: strField('An optional key so a repeated submission returns the original answer.'),
      ...WRITE_PROPERTIES,
    },
    ['name', 'category', 'currency'],
  ),
  schema: z
    .object({
      name: z.string().min(1).max(64),
      category: z.enum(ACCOUNT_CATEGORIES),
      currency: zCurrency,
      initial_balance: hundredths().optional(),
      initial_balance_date: zDate.optional(),
      color: z.string().optional(),
      icon: z.string().optional(),
      comment: z.string().optional(),
      credit_card_statement_date: z.number().int().min(0).max(28).optional(),
      credit_card_limit: hundredths().optional(),
      sub_accounts: z
        .array(
          z
            .object({ name: z.string().min(1).max(64), currency: zCurrency, initial_balance: hundredths().optional(), initial_balance_date: zDate.optional(), color: z.string().optional(), icon: z.string().optional(), comment: z.string().optional() })
            .strict(),
        )
        .optional(),
      idempotency_key: z.string().max(128).optional(),
      ...zWrite,
    })
    .strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/accounts', { method: 'POST', body: { ...withoutWriteKeys(a), ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

export const updateAccount: ToolDef = {
  name: 'ezb_update_account',
  route: { method: 'PATCH', path: '/accounts/:id' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: "Edits one account's name, comment, colour, icon, credit-card statement date or credit limit, or hides / shows it (hidden, on its own: a hidden account drops out of the net-worth total and the default lists but keeps its balance); currency and category are immutable (a new account plus a transfer is the honest move).",
    tier: 'write',
    insteadOf: 'To find the account, use ezb_list_accounts.',
  }),
  inputSchema: objectSchema({
    ...ACCOUNT_REF_PROPERTIES,
    name: strField('The new name.'),
    color: strField('A hex RGB colour such as 000000.'),
    icon: strField('An icon id.'),
    comment: strField('The new comment.'),
    credit_card_statement_date: intField('For a credit card: the day of the month the statement closes.', { minimum: 0, maximum: 28 }),
    credit_card_limit: amountField('For a credit card: the credit limit.'),
    hidden: boolField('true hides the account, false shows it again. Send it on its own, without the other edit fields.'),
    ...WRITE_PROPERTIES,
  }),
  schema: z
    .object({
      ...zAccountRef,
      hidden: z.boolean().optional(),
      name: z.string().min(1).max(64).optional(),
      color: z.string().optional(),
      icon: z.string().optional(),
      comment: z.string().optional(),
      credit_card_statement_date: z.number().int().min(0).max(28).optional(),
      credit_card_limit: hundredths().optional(),
      ...zWrite,
    })
    .strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & { account_id?: string; account_name?: string } & Record<string, unknown>;
    const { account_id: _id, account_name: _n, hidden, ...rest } = withoutWriteKeys(a);
    // hiding is its own plane route (POST /accounts/:id/hide), so it cannot share a preview with a PATCH
    const hide = hidden !== undefined;
    if (hide && Object.keys(rest).length > 0) toolFail('invalid_input', 'send hidden on its own, without the other edit fields', 'make two calls: one with hidden, one with the edits');
    const path = `/accounts/${accountSegment(a)}${hide ? '/hide' : ''}`;
    const body = hide ? { hidden, ...writeOpts(a, ctx.config) } : { ...rest, ...writeOpts(a, ctx.config) };
    const res = await ctx.client.request(path, { method: hide ? 'POST' : 'PATCH', body });
    return fromWrite(res);
  },
};

export const addCategory: ToolDef = {
  name: 'ezb_add_category',
  route: { method: 'POST', path: '/categories' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Adds a category: a primary one (name and type, no parent) or a secondary one under a primary (parent_id or parent_name; the type is inherited) — transactions use secondary categories.',
    tier: 'write',
    insteadOf: 'Check ezb_list_categories first so a near-duplicate is not created.',
  }),
  inputSchema: objectSchema(
    {
      name: strField('The category name (up to 64 characters).'),
      type: enumField('The type, for a primary category (omit under a parent).', CATEGORY_TYPES),
      parent_id: idField('The primary category to nest under, for a secondary category.'),
      parent_name: strField('The primary category by name instead of id.'),
      color: strField('A hex RGB colour such as 000000.'),
      icon: strField('An icon id.'),
      comment: strField('A comment.'),
      idempotency_key: strField('An optional key so a repeated submission returns the original answer.'),
      ...WRITE_PROPERTIES,
    },
    ['name'],
  ),
  schema: z
    .object({
      name: z.string().min(1).max(64),
      type: z.enum(CATEGORY_TYPES).optional(),
      parent_id: zId.optional(),
      parent_name: z.string().min(1).optional(),
      color: z.string().optional(),
      icon: z.string().optional(),
      comment: z.string().optional(),
      idempotency_key: z.string().max(128).optional(),
      ...zWrite,
    })
    .strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/categories', { method: 'POST', body: { ...withoutWriteKeys(a), ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

export const addCategories: ToolDef = {
  name: 'ezb_add_categories',
  route: { method: 'POST', path: '/categories/ensure' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Makes sure a list of category paths "Type > Group > Sub" exists, creating only the missing groups and sub-categories (matching names ignore case) and reporting the ids of the ones already there — the taxonomy a categoriser needs, in one undoable write.',
    tier: 'write',
    insteadOf: 'For one category with a colour, icon or comment, ezb_add_category. Check ezb_list_categories first so a near-duplicate spelling is not created.',
  }),
  inputSchema: objectSchema(
    {
      paths: { type: 'array', minItems: 1, items: { type: 'string' }, description: 'Full paths, e.g. "Expense > Food & Drink > Coffee", "Income > Occupational Earnings > Salary Income".' },
      color: strField('A hex RGB colour for new groups; a new sub-category takes its group\'s.'),
      icon: strField('An icon id for new groups; a new sub-category takes its group\'s.'),
      ...WRITE_PROPERTIES,
    },
    ['paths'],
  ),
  schema: z.object({ paths: z.array(z.string().min(1)).min(1).max(1000), color: z.string().optional(), icon: z.string().optional(), ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/categories/ensure', { method: 'POST', body: { ...withoutWriteKeys(a), ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

export const updateCategory: ToolDef = {
  name: 'ezb_update_category',
  route: { method: 'PATCH', path: '/categories/:id' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: "Edits one category: renames it, moves a sub-category to another group of the same type (parent_id or parent_name), or changes its colour, icon or comment; every transaction in it follows, and the preview is the field-by-field before and after.",
    tier: 'write',
    insteadOf: 'A category cannot change its major category (income, expense, transfer): create the path under the other type with ezb_add_categories and move the rows with ezb_set_transaction_categories.',
  }),
  inputSchema: objectSchema(
    {
      category_id: idField('The category to edit.'),
      category_name: strField('The category by name or "Group > Sub" instead of id.'),
      name: strField('The new name (up to 64 characters).'),
      parent_id: idField('For a sub-category: the group to move it to (same type).'),
      parent_name: strField('The group to move it to, by name.'),
      color: strField('A hex RGB colour such as 000000.'),
      icon: strField('An icon id.'),
      comment: strField('A comment.'),
      ...WRITE_PROPERTIES,
    },
  ),
  schema: z
    .object({
      category_id: zId.optional(),
      category_name: z.string().min(1).optional(),
      name: z.string().min(1).max(64).optional(),
      parent_id: zId.optional(),
      parent_name: z.string().min(1).optional(),
      color: z.string().optional(),
      icon: z.string().optional(),
      comment: z.string().optional(),
      ...zWrite,
    })
    .strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown> & { category_id?: string; category_name?: string };
    const ref = a.category_id ?? a.category_name;
    if (ref === undefined) {
      toolFail('invalid_input', 'pass category_id or category_name', 'ezb_list_categories lists them');
    }
    const { category_id: _i, category_name: _n, ...rest } = withoutWriteKeys(a);
    const res = await ctx.client.request(`/categories/${seg(ref)}`, { method: 'PATCH', body: { ...rest, ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

export const addTag: ToolDef = {
  name: 'ezb_add_tag',
  route: { method: 'POST', path: '/tags' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Adds a tag, optionally inside a tag group.',
    tier: 'write',
    insteadOf: 'Check ezb_list_tags first so a near-duplicate is not created; to put it on transactions, ezb_add_transaction_tags.',
  }),
  inputSchema: objectSchema(
    {
      name: strField('The tag name.'),
      group_id: idField('The tag group to put it in.'),
      group_name: strField('The tag group by name instead of id.'),
      idempotency_key: strField('An optional key so a repeated submission returns the original answer.'),
      ...WRITE_PROPERTIES,
    },
    ['name'],
  ),
  schema: z.object({ name: z.string().min(1), group_id: zId.optional(), group_name: z.string().min(1).optional(), idempotency_key: z.string().max(128).optional(), ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/tags', { method: 'POST', body: { ...withoutWriteKeys(a), ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

// ─── scheduled transactions ──────────────────────────────────────────────────────────────────

const FREQUENCIES = ['daily', 'weekly', 'monthly', 'yearly', 'every_n_days', 'disabled'] as const;

const SCHEDULE_PROPERTIES: Record<string, unknown> = {
  name: strField('The schedule name ("Rent").'),
  type: enumField('The transaction type it creates.', TXN_WRITE_TYPES),
  account_id: idField('The account (for a transfer, the source).'),
  account_name: strField('The account by name.'),
  destination_account_id: idField('For a transfer: the destination account.'),
  destination_account_name: strField('For a transfer: the destination by name.'),
  amount: amountField('The amount it creates, as a positive magnitude.'),
  destination_amount: amountField('For a cross-currency transfer: the amount received.'),
  category_id: idField('The secondary category.'),
  category_name: strField('The secondary category by name.'),
  tag_ids: idListField('Tags to attach.'),
  tag_names: { type: 'array', items: { type: 'string' }, description: 'Tags to attach, by name.' },
  comment: strField('The comment each created transaction carries.'),
  hide_amount: boolField('Hide the amount in the UI.'),
  frequency: enumField('How often it fires; every_n_days uses frequency_value as the interval, disabled pauses it.', FREQUENCIES),
  frequency_value: {
    description: 'For weekly: the weekdays (0-6, Sunday 0) as a list; for monthly: the days of the month (1-31) as a list; for every_n_days: the number of days; for yearly: the day of year. The plane echoes what it resolved.',
    anyOf: [{ type: 'integer' }, { type: 'string' }, { type: 'array', items: { type: 'integer' } }],
  },
  start: dateField('The first day the schedule may fire.'),
  end: dateField('The last day it may fire; "" removes the end date on an update.'),
  timezone: strField('The IANA timezone the schedule fires in. Defaults to the resolved one.'),
};

const zSchedule = {
  name: z.string().min(1).optional(),
  type: z.enum(TXN_WRITE_TYPES).optional(),
  account_id: zId.optional(),
  account_name: z.string().min(1).optional(),
  destination_account_id: zId.optional(),
  destination_account_name: z.string().min(1).optional(),
  amount: hundredths().optional(),
  destination_amount: hundredths().optional(),
  category_id: zId.optional(),
  category_name: z.string().min(1).optional(),
  tag_ids: z.array(zId).max(10).optional(),
  tag_names: z.array(z.string().min(1)).max(10).optional(),
  comment: z.string().optional(),
  hide_amount: z.boolean().optional(),
  frequency: z.enum(FREQUENCIES).optional(),
  frequency_value: z.union([z.number().int(), z.string(), z.array(z.number().int())]).optional(),
  start: zDate.optional(),
  end: z.union([zDate, z.literal('')]).optional(),
  timezone: z.string().min(1).optional(),
};

export const addScheduledTransaction: ToolDef = {
  name: 'ezb_add_scheduled_transaction',
  route: { method: 'POST', path: '/templates' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: "Adds a scheduled transaction — rent on the 1st — that the app's cron turns into real transactions on its frequency from its start date; the preview shows the template and its next occurrence.",
    tier: 'write',
    insteadOf: 'After applying, ezb_list_upcoming_schedules shows the next dates. To see what already exists, ezb_list_templates with kind scheduled.',
  }),
  inputSchema: objectSchema({ ...SCHEDULE_PROPERTIES, idempotency_key: strField('An optional key so a repeated submission returns the original answer.'), ...WRITE_PROPERTIES }, ['name', 'type', 'amount', 'frequency']),
  schema: z.object({ ...zSchedule, name: z.string().min(1), type: z.enum(TXN_WRITE_TYPES), amount: hundredths(), frequency: z.enum(FREQUENCIES), idempotency_key: z.string().max(128).optional(), ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & Record<string, unknown>;
    const res = await ctx.client.request('/templates', { method: 'POST', body: { kind: 'scheduled', ...withoutWriteKeys(a), ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

export const updateScheduledTransaction: ToolDef = {
  name: 'ezb_update_scheduled_transaction',
  route: { method: 'PATCH', path: '/templates/:id' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: 'Edits a scheduled transaction — any of its fields; absent means unchanged — or pauses it with frequency disabled and resumes it with a real frequency; the preview is the before and after.',
    tier: 'write',
    insteadOf: 'To find it, ezb_list_templates with kind scheduled.',
  }),
  inputSchema: objectSchema(
    {
      template_id: idField('The schedule to edit.'),
      template_name: strField('The schedule by name instead of id.'),
      ...SCHEDULE_PROPERTIES,
      ...WRITE_PROPERTIES,
    },
  ),
  schema: z.object({ template_id: zId.optional(), template_name: z.string().min(1).optional(), ...zSchedule, ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & { template_id?: string; template_name?: string } & Record<string, unknown>;
    const ref = a.template_id ?? a.template_name;
    if (ref === undefined) {
      toolFail('invalid_input', 'pass template_id or template_name', 'ezb_list_templates with kind scheduled lists them');
    }
    const { template_id: _i, template_name: _n, ...rest } = withoutWriteKeys(a);
    const res = await ctx.client.request(`/templates/${seg(ref)}`, { method: 'PATCH', body: { ...rest, ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

// ─── exchange rates, reconcile, undo ─────────────────────────────────────────────────────────

export const setCustomExchangeRate: ToolDef = {
  name: 'ezb_set_custom_exchange_rate',
  route: { method: 'PUT', path: '/exchange-rates/custom/:currency' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: "Sets the operator's own exchange rate for a currency — units of that currency per ONE unit of their default currency, as a decimal string — which the app uses in place of the provider's; the preview says whether the app's rate source is set to honour custom rates.",
    tier: 'write',
    insteadOf: 'Read the current rate first with ezb_get_exchange_rates. A rate is a ratio, the one non-integer on this server.',
  }),
  inputSchema: objectSchema(
    {
      currency: currencyField('The currency the rate is for.'),
      rate: strField("The rate as a decimal string, e.g. \"1.08\": units of this currency per one unit of the operator's default currency."),
      ...WRITE_PROPERTIES,
    },
    ['currency', 'rate'],
  ),
  schema: z.object({ currency: zCurrency, rate: z.string().regex(/^\d+(\.\d+)?$/, 'rate is a positive decimal string such as "1.08"'), ...zWrite }).strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & { currency: string; rate: string };
    const res = await ctx.client.request(`/exchange-rates/custom/${seg(a.currency)}`, { method: 'PUT', body: { rate: a.rate, ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

export const applyReconcile: ToolDef = {
  name: 'ezb_apply_reconcile',
  route: { method: 'POST', path: '/accounts/:id/reconcile/apply' },
  tier: 'write',
  hasDryRun: true,
  description: describe({
    what: "Marks an account reconciled as of a date and, only if create_adjustment is asked for, first adds ONE visible income or expense transaction for the difference in the named category — never a hidden plug, never an opening-balance row; pass the same arguments the plan took.",
    tier: 'write',
    insteadOf: 'Call ezb_plan_reconcile first, and if the difference is not zero hunt it before adjusting; its confirm_token is what this needs.',
  }),
  inputSchema: objectSchema(
    {
      ...ACCOUNT_REF_PROPERTIES,
      target_balance: amountField("The balance the operator's statement shows — the same figure the plan took."),
      as_of: dateField('The date the statement balance is for. Defaults to today.'),
      create_adjustment: boolField('Add one visible adjustment transaction for the difference. Defaults to false: find the missing transaction instead.'),
      adjustment_category_id: idField('The secondary category the adjustment goes in, when create_adjustment is true.'),
      adjustment_category_name: strField('The adjustment category by name.'),
      adjustment_comment: strField('The comment on the adjustment.'),
      mark_reconciled: boolField("Set the account's last reconciled time. Defaults to true."),
      ...WRITE_PROPERTIES,
    },
    ['target_balance'],
  ),
  schema: z
    .object({
      ...zAccountRef,
      target_balance: hundredths(),
      as_of: zDate.optional(),
      create_adjustment: z.boolean().optional(),
      adjustment_category_id: zId.optional(),
      adjustment_category_name: z.string().min(1).optional(),
      adjustment_comment: z.string().optional(),
      mark_reconciled: z.boolean().optional(),
      ...zWrite,
    })
    .strict(),
  async run(args, ctx) {
    const a = args as WriteArgs & { account_id?: string; account_name?: string } & Record<string, unknown>;
    const { account_id: _id, account_name: _n, ...rest } = withoutWriteKeys(a);
    const res = await ctx.client.request(`/accounts/${accountSegment(a)}/reconcile/apply`, { method: 'POST', body: { ...rest, ...writeOpts(a, ctx.config) } });
    return fromWrite(res);
  },
};

export const undo: ToolDef = {
  name: 'ezb_undo',
  route: { method: 'POST', path: '/undo' },
  tier: 'write',
  hasDryRun: false,
  description: describe({
    what: 'Reverses the most recent machine-plane write of the bound user — made through this server or the CLI, never something done in the browser — and reports exactly what it reversed; it has no dry run because the report is the preview, and it refuses with conflict rather than overwrite a row the operator has edited since.',
    tier: 'write',
    insteadOf: 'ezb_list_journal shows what it would undo next. Offer this first after any write that was not what the operator meant.',
  }),
  inputSchema: objectSchema({
    journal_id: strField('A guard: only undo if this journal entry (from ezb_list_journal) is the one next in line; a retry after it was already undone then answers undone false instead of undoing the next entry.'),
  }),
  schema: z.object({ journal_id: z.string().regex(/^\d+$/).optional() }).strict(),
  async run(args, ctx) {
    const a = args as { journal_id?: string };
    const res = await ctx.client.request('/undo', { method: 'POST', body: a.journal_id === undefined ? {} : { journal_id: a.journal_id } });
    return fromWrite(res);
  },
};

export const WRITE_TOOLS: ToolDef[] = [
  applyAccounts,
  applyStatementImport,
  applyFileImport,
  addTransactions,
  updateTransaction,
  setTransactionCategory,
  setTransactionCategories,
  setTransactionAccount,
  addTransactionTags,
  removeTransactionTags,
  addAccount,
  updateAccount,
  addCategory,
  addCategories,
  updateCategory,
  addTag,
  addScheduledTransaction,
  updateScheduledTransaction,
  setCustomExchangeRate,
  applyReconcile,
  undo,
];
