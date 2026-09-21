/**
 * Statements and ingest — pm/mcp.mdx §9.5, §11. Eleven read tools over the machine plane's ingest
 * engine (apis.mdx §14–15). The archive is read-only; the plane's import record is the only
 * authority on "already imported"; the model never judges a duplicate, a mapping or a transfer
 * pairing — every one of those arrives as a question for the operator.
 */
import { z } from 'zod';

import { ACCOUNT_REF_PROPERTIES, zAccountRef } from './accounts.js';
import { listImportFallout } from './analytics.js';
import { boolField, clampLimit, dateField, describe, enumField, fromPlane, intField, objectSchema, strField, zDate, zId } from './tool.js';
import type { ToolContext, ToolDef, ToolResult } from './tool.js';

const STATEMENT_UNTRUSTED = ['description', 'comment', 'label', 'name', 'entity', 'institution'];

const ARCHIVE_READ_ONLY = 'The archive itself is read-only: nothing under the statements root is ever written, moved or renamed.';

/** `root` is optional everywhere: the plane falls back to ezbookkeeping.statements.root in the credentials file. */
const ROOT_PROPERTIES: Record<string, unknown> = {
  root: strField('The statements root directory. Defaults to ezbookkeeping.statements.root in the credentials file (or EZBK_STATEMENTS_DIR).'),
  manifest_path: strField('The manifest file, relative to the root. Defaults to the one the plane finds.'),
};
const zRoot = { root: z.string().min(1).optional(), manifest_path: z.string().min(1).optional() };
type RootArgs = { root?: string; manifest_path?: string };

const MODES = ['prepared', 'raw'] as const;

/**
 * The statements root when the call did not name one: EZBK_STATEMENTS_DIR, shared with the CLI
 * (§14). The plane's own fallback is ezbookkeeping.statements.root in the credentials file; the CLI
 * passes its --path / EZBK_STATEMENTS_DIR as `root`, and so does this server.
 */
export function withRoot<T extends { root?: string | undefined }>(args: T, ctx: ToolContext): T {
  if (args.root === undefined && ctx.config.statementsDir !== undefined) {
    return { ...args, root: ctx.config.statementsDir };
  }
  return args;
}
const QIF_ORDERS = ['ymd', 'mdy', 'dmy'] as const;

/** Optional `staging` on the routes that take it (never inside the manifest's own directory). */
const STAGING = strField('The staging directory. Defaults to {root}/.ezbk-staging; never the archive\'s own import/ directory.');

export const getStatementManifest: ToolDef = {
  name: 'ezb_get_statement_manifest',
  route: { method: 'GET', path: '/ingest/manifest' },
  tier: 'read',
  description: describe({
    what: "CALL THIS FIRST in any import conversation: reads what the statement archive says about itself — prepared or raw mode, every account with its entity, institution, last-four, kind, currency, counts and date range, which converter each file gets, and the archive's own warnings; missing manifest columns are reported, never guessed.",
    tier: 'read',
    insteadOf: 'In a prepared archive the files already carry bank ids; do not reach for ezb_extract_statements there. ' + ARCHIVE_READ_ONLY,
  }),
  inputSchema: objectSchema(ROOT_PROPERTIES),
  schema: z.object(zRoot).strict(),
  async run(args, ctx) {
    const a = withRoot(args as RootArgs, ctx);
    const res = await ctx.client.request('/ingest/manifest', { query: { root: a.root, manifest_path: a.manifest_path } });
    return fromPlane(res, STATEMENT_UNTRUSTED);
  },
};

export const scanStatements: ToolDef = {
  name: 'ezb_scan_statements',
  route: { method: 'POST', path: '/ingest/scan' },
  tier: 'read',
  description: describe({
    what: 'Walks the statement archive and classifies every file into entity, institution, account and month — statements per account-month, missing months, duplicate scans, unusable files — grouped and de-duplicated at the statement level, changing nothing.',
    tier: 'read',
    insteadOf: 'For just the gaps use ezb_list_missing_statements; for what was collapsed and why, ezb_list_statement_duplicates. ' + ARCHIVE_READ_ONLY,
  }),
  inputSchema: objectSchema({
    ...ROOT_PROPERTIES,
    mode: enumField('Force prepared or raw mode. Defaults to what the manifest says.', MODES),
    entity: strField('Only this entity.'),
    institution: strField('Only this institution.'),
    account: strField('Only this account (its label or account key).'),
    year: { type: 'string', pattern: '^\\d{4}$', description: 'Only this year.' },
    qif_date_order: enumField('The date order for .qif files, which the plane refuses to guess.', QIF_ORDERS),
    statements: boolField('Include every statement file in the answer, not just the counts.'),
  }),
  schema: z
    .object({
      ...zRoot,
      mode: z.enum(MODES).optional(),
      entity: z.string().min(1).optional(),
      institution: z.string().min(1).optional(),
      account: z.string().min(1).optional(),
      year: z.string().regex(/^\d{4}$/).optional(),
      qif_date_order: z.enum(QIF_ORDERS).optional(),
      statements: z.boolean().optional(),
    })
    .strict(),
  async run(args, ctx) {
    const res = await ctx.client.request('/ingest/scan', { method: 'POST', body: withRoot(args as RootArgs, ctx) });
    return fromPlane(res, STATEMENT_UNTRUSTED);
  },
};

export const listMissingStatements: ToolDef = {
  name: 'ezb_list_missing_statements',
  route: { method: 'GET', path: '/ingest/coverage' },
  tier: 'read',
  description: describe({
    what: 'Lists which account-months are present in the archive and which are missing — just the gaps.',
    tier: 'read',
    insteadOf: 'For the whole tree, use ezb_scan_statements. ' + ARCHIVE_READ_ONLY,
  }),
  inputSchema: objectSchema({
    ...ROOT_PROPERTIES,
    account: strField('Only this account (its label or account key).'),
    from_rows: boolField('Judge coverage from the parsed rows rather than from the statement files. Defaults to false.'),
  }),
  schema: z.object({ ...zRoot, account: z.string().min(1).optional(), from_rows: z.boolean().optional() }).strict(),
  async run(args, ctx) {
    const a = withRoot(args as RootArgs & { account?: string; from_rows?: boolean }, ctx);
    const res = await ctx.client.request('/ingest/coverage', { query: { root: a.root, manifest_path: a.manifest_path, account: a.account, from_rows: a.from_rows } });
    return fromPlane(res, STATEMENT_UNTRUSTED);
  },
};

export const listStatementDuplicates: ToolDef = {
  name: 'ezb_list_statement_duplicates',
  route: { method: 'GET', path: '/ingest/dupes' },
  tier: 'read',
  description: describe({
    what: 'Lists what both de-duplication layers collapsed and the rule that decided each: identical statements, superseded ones, same-bank-id row collapses, and the conflicts — two statements for one account-month that disagree about which transactions exist, which block that account until a human picks.',
    tier: 'read',
    insteadOf: 'On a conflict, show the operator both files and ask which is correct; do not choose, and there is no tool that merges or deletes.',
  }),
  inputSchema: objectSchema({
    ...ROOT_PROPERTIES,
    mode: enumField('Force prepared or raw mode.', MODES),
    account: strField('Only this account.'),
    prefer: { type: 'array', items: { type: 'string' }, description: 'Statement paths the operator has chosen to win a conflict, relative to the root.' },
    date_order: enumField('The date order for ambiguous files.', QIF_ORDERS),
  }),
  schema: z.object({ ...zRoot, mode: z.enum(MODES).optional(), account: z.string().min(1).optional(), prefer: z.array(z.string().min(1)).optional(), date_order: z.enum(QIF_ORDERS).optional() }).strict(),
  async run(args, ctx) {
    const a = withRoot(args as RootArgs & { mode?: string; account?: string; prefer?: string[]; date_order?: string }, ctx);
    const res = await ctx.client.request('/ingest/dupes', { query: { root: a.root, manifest_path: a.manifest_path, mode: a.mode, account: a.account, prefer: a.prefer, date_order: a.date_order } });
    return fromPlane(res, STATEMENT_UNTRUSTED);
  },
};

export const getStatementRows: ToolDef = {
  name: 'ezb_get_statement_rows',
  route: { method: 'GET', path: '/ingest/rows' },
  tier: 'read',
  description: describe({
    what: "Gets the de-duplicated parsed rows of one account over a range, each with its import_id and its verdict against the books (new, already present, deleted since, possible match) — parsed rows, never the raw text of a statement.",
    tier: 'read',
    insteadOf: 'For the whole plan across accounts use ezb_plan_statement_import.',
  }),
  inputSchema: objectSchema(
    {
      ...ROOT_PROPERTIES,
      account: strField('The account (its label or account key).'),
      start: dateField('First day, inclusive.'),
      end: dateField('Last day, inclusive.'),
      status: strField('Only rows with this verdict (new, already_present, already_present_deleted, possible_match, superseded).'),
      limit: intField('Rows to return; clamped to this server\'s cap.', { minimum: 1 }),
      offset: intField('Rows to skip.', { minimum: 0 }),
    },
    ['account'],
  ),
  schema: z.object({ ...zRoot, account: z.string().min(1), start: zDate.optional(), end: zDate.optional(), status: z.string().min(1).optional(), limit: z.number().int().positive().optional(), offset: z.number().int().min(0).optional() }).strict(),
  async run(args, ctx) {
    const a = withRoot(args as RootArgs & { account: string; start?: string; end?: string; status?: string; limit?: number; offset?: number }, ctx);
    const { limit, clamped } = clampLimit(a.limit, ctx.config);
    const res = await ctx.client.request('/ingest/rows', { query: { root: a.root, manifest_path: a.manifest_path, account: a.account, start: a.start, end: a.end, status: a.status, limit, offset: a.offset } });
    const out: ToolResult = fromPlane(res, STATEMENT_UNTRUSTED);
    if (clamped) {
      out.truncated = true;
      out.limitApplied = limit;
    }
    return out;
  },
};

export const describeStatementMap: ToolDef = {
  name: 'ezb_describe_statement_map',
  route: { method: 'GET', path: '/ingest/map' },
  tier: 'read',
  description: describe({
    what: "Describes the statements-to-books map: which ezBookkeeping account each archive account key resolves to, whether each mapping is still usable, and which manifest accounts are not mapped yet.",
    tier: 'read',
    insteadOf: 'To create the missing accounts and write the map, use ezb_plan_accounts then ezb_apply_accounts.',
  }),
  inputSchema: objectSchema(ROOT_PROPERTIES),
  schema: z.object(zRoot).strict(),
  async run(args, ctx) {
    const a = withRoot(args as RootArgs, ctx);
    const res = await ctx.client.request('/ingest/map', { query: { root: a.root, manifest_path: a.manifest_path } });
    return fromPlane(res, STATEMENT_UNTRUSTED);
  },
};

export const extractStatements: ToolDef = {
  name: 'ezb_extract_statements',
  route: { method: 'POST', path: '/ingest/extract' },
  tier: 'read',
  noTimeout: true,
  description: describe({
    what: "RAW MODE ONLY: turns statement PDFs and their text sidecars into ezBookkeeping CSV per account-month in the staging directory, with a conservative parser that names every zero-row statement and unreadable line; it never touches the books and writes only to staging.",
    tier: 'read',
    insteadOf: 'In a prepared archive (see ezb_get_statement_manifest) there is nothing to extract — go straight to ezb_plan_statement_import. ' + ARCHIVE_READ_ONLY,
  }),
  inputSchema: objectSchema({
    ...ROOT_PROPERTIES,
    staging: STAGING,
    force: boolField('Re-extract statements that already have staged output. Defaults to false.'),
    date_order: enumField('The date order to read ambiguous dates with.', QIF_ORDERS),
    accounts: { type: 'array', items: { type: 'string' }, description: 'Only these accounts (labels or account keys).' },
    prefer: { type: 'array', items: { type: 'string' }, description: 'Statement paths the operator has chosen to win a conflict.' },
    max_statements: intField('Stop after this many statements. Defaults to 5000.', { minimum: 1 }),
  }),
  schema: z
    .object({
      ...zRoot,
      staging: z.string().min(1).optional(),
      force: z.boolean().optional(),
      date_order: z.enum(QIF_ORDERS).optional(),
      accounts: z.array(z.string().min(1)).optional(),
      prefer: z.array(z.string().min(1)).optional(),
      max_statements: z.number().int().positive().optional(),
    })
    .strict(),
  async run(args, ctx) {
    const res = await ctx.client.request('/ingest/extract', { method: 'POST', body: withRoot(args as RootArgs, ctx), noTimeout: true });
    return fromPlane(res, STATEMENT_UNTRUSTED);
  },
};

const KINDS = ['checking', 'savings', 'card', 'brokerage', 'loan', 'cash', 'cd'] as const;

export const planAccounts: ToolDef = {
  name: 'ezb_plan_accounts',
  route: { method: 'POST', path: '/ingest/accounts/plan' },
  tier: 'read',
  description: describe({
    what: "Plans the accounts a statement archive needs, one decision per manifest row — create, link, skip or ambiguous — with the proposed name, the ezBookkeeping category and its asset-or-liability side, and the currency, and returns the confirm_token for ezb_apply_accounts; it creates nothing. Before applying: show every ambiguous row by name and stop; read out every liability (cards, loans) and every currency, because a card created as checking inverts its balance and a currency cannot be changed later; never propose names of your own.",
    tier: 'read',
    insteadOf: 'Then ezb_apply_accounts with the token. For the current map use ezb_describe_statement_map.',
  }),
  inputSchema: objectSchema({
    ...ROOT_PROPERTIES,
    staging: STAGING,
    naming: strField('The name template. Defaults to "{Entity} · {Institution} {Kind} ••{last4}", which is stable so a re-run links instead of duplicating.'),
    kind_to_category: {
      type: 'object',
      additionalProperties: { type: 'string' },
      description: `Override the manifest kind to ezBookkeeping category map (kinds: ${KINDS.join(', ')}; categories: checking, savings, cash, credit_card, investment, debt, certificate_of_deposit). Echoed and explained per row.`,
    },
    overrides: {
      type: 'object',
      additionalProperties: {
        type: 'object',
        properties: { name: { type: 'string' }, action: { type: 'string', enum: ['skip', 'link', 'create'] }, account_id: { type: 'string' } },
        additionalProperties: false,
      },
      description: "Per account key, the operator's resolution: a name, an action (skip, link, create), and for link the account_id to link to. This is how an ambiguous row is resolved — by the operator, never by you.",
    },
    accounts: { type: 'array', items: { type: 'string' }, description: 'Only these manifest accounts (labels or account keys).' },
  }),
  schema: z
    .object({
      ...zRoot,
      staging: z.string().min(1).optional(),
      naming: z.string().min(1).optional(),
      kind_to_category: z.record(z.string(), z.string()).optional(),
      overrides: z.record(z.string(), z.object({ name: z.string().optional(), action: z.enum(['skip', 'link', 'create']).optional(), account_id: zId.optional() }).strict()).optional(),
      accounts: z.array(z.string().min(1)).optional(),
    })
    .strict(),
  async run(args, ctx) {
    const res = await ctx.client.request('/ingest/accounts/plan', { method: 'POST', body: withRoot(args as RootArgs, ctx) });
    return fromPlane(res, STATEMENT_UNTRUSTED);
  },
};

/** The plan arguments shared by ezb_plan_statement_import and ezb_apply_statement_import. Note: no reimport_deleted (§11.3). */
export const PLAN_IMPORT_PROPERTIES: Record<string, unknown> = {
  ...ROOT_PROPERTIES,
  staging: STAGING,
  mode: enumField('Force prepared or raw mode.', MODES),
  accounts: { type: 'array', items: { type: 'string' }, description: 'Only these archive accounts (labels or account keys).' },
  start: dateField('Only rows from this day, inclusive.'),
  end: dateField('Only rows up to this day, inclusive.'),
  category_map: {
    type: 'object',
    additionalProperties: { type: 'string' },
    description: 'Statement category name to ezBookkeeping secondary category id. Unmatched names are reported in unmapped[]; ask the operator which to use.',
  },
  fallback_category_ids: {
    type: 'object',
    properties: { expense: { type: 'string' }, income: { type: 'string' } },
    additionalProperties: false,
    description: 'Where unmapped rows land, per side, as secondary category ids the OPERATOR named ({expense, income}). Without it a plan with unmapped categories blocks that account. ezb_list_import_fallout later lists what went here.',
  },
  accept_matches: {
    type: 'array',
    items: { type: 'object', properties: { import_id: { type: 'string' }, row_id: { type: 'string' }, transaction_id: { type: 'string' } }, required: ['transaction_id'], additionalProperties: false },
    description: "Possible matches the operator accepted: link this statement row to this hand-entered transaction (writing the record, creating nothing). Never accept one on your own.",
  },
  accept_transfers: {
    type: 'array',
    items: { type: 'string' },
    description: 'Transfer candidates the operator accepted, by candidate id: a card payment in both statements imports as ONE transfer instead of an expense plus an income. Unaccepted, both sides import and spending is double-counted; the plan says so.',
  },
  prefer: { type: 'array', items: { type: 'string' }, description: 'Statement paths the operator has chosen to win a conflict.' },
  qif_date_order: enumField('The date order for .qif files, which the plane refuses to guess.', QIF_ORDERS),
  include_rows: boolField('Include the new rows themselves in the plan, not only the counts. Defaults to false.'),
  rows_limit: intField('How many rows to include when include_rows is true.', { minimum: 1 }),
};

export const zPlanImport = {
  ...zRoot,
  staging: z.string().min(1).optional(),
  mode: z.enum(MODES).optional(),
  accounts: z.array(z.string().min(1)).optional(),
  start: zDate.optional(),
  end: zDate.optional(),
  category_map: z.record(z.string(), zId).optional(),
  fallback_category_ids: z.object({ expense: zId.optional(), income: zId.optional() }).strict().optional(),
  accept_matches: z.array(z.object({ import_id: z.string().min(1).optional(), row_id: z.string().min(1).optional(), transaction_id: zId }).strict()).optional(),
  accept_transfers: z.array(z.string().min(1)).optional(),
  prefer: z.array(z.string().min(1)).optional(),
  qif_date_order: z.enum(QIF_ORDERS).optional(),
  include_rows: z.boolean().optional(),
  rows_limit: z.number().int().positive().optional(),
};

export const planStatementImport: ToolDef = {
  name: 'ezb_plan_statement_import',
  route: { method: 'POST', path: '/ingest/plan' },
  tier: 'read',
  description: describe({
    what: "Plans the import of the statement archive into the books, per account: rows parsed, collapsed, already imported, already imported but deleted since (kept deleted — no argument re-adds them), possible matches with hand-entered rows, transfer candidates, unmapped categories, and the NEW rows — plus the confirm_token for ezb_apply_statement_import. The plane's own import record decides what is already present; show the unmapped names, the transfer candidates and any conflict to the operator and ask.",
    tier: 'read',
    insteadOf: 'Call ezb_get_statement_manifest first; then ezb_apply_statement_import with the token. For a single downloaded file use ezb_plan_file_import.',
  }),
  inputSchema: objectSchema(PLAN_IMPORT_PROPERTIES),
  schema: z.object(zPlanImport).strict(),
  async run(args, ctx) {
    const res = await ctx.client.request('/ingest/plan', { method: 'POST', body: withRoot(args as RootArgs, ctx), noTimeout: true });
    return fromPlane(res, STATEMENT_UNTRUSTED);
  },
};

/** The one-file plan arguments shared by ezb_plan_file_import and ezb_apply_file_import. */
export const PLAN_FILE_PROPERTIES: Record<string, unknown> = {
  ...ACCOUNT_REF_PROPERTIES,
  root: strField('The statements root the file path is relative to. Defaults to the configured root.'),
  staging: STAGING,
  path: strField('The file to import, relative to the root — the OFX, QFX, QIF, CAMT, MT940 or CSV the operator downloaded.'),
  file_type: strField("The ezBookkeeping converter to use (ofx, qfx, qif_ymd, qif_mdy, qif_dmy, camt052, camt053, mt940, ezbookkeeping_csv, custom_csv …; ezb_capabilities and the plan list them). Defaults to what the extension and content say; a .qif needs the date order stated."),
  column_map: { type: 'object', description: 'For custom delimiter-separated files: the column map the Import dialog would ask for.' },
  start: dateField('Only rows from this day, inclusive.'),
  end: dateField('Only rows up to this day, inclusive.'),
  category_map: { type: 'object', additionalProperties: { type: 'string' }, description: 'Statement category name to ezBookkeeping secondary category id.' },
  fallback_category_ids: {
    type: 'object',
    properties: { expense: { type: 'string' }, income: { type: 'string' } },
    additionalProperties: false,
    description: 'Where unmapped rows land, per side, as secondary category ids the operator named.',
  },
  accept_matches: {
    type: 'array',
    items: { type: 'object', properties: { import_id: { type: 'string' }, row_id: { type: 'string' }, transaction_id: { type: 'string' } }, required: ['transaction_id'], additionalProperties: false },
    description: 'Possible matches the operator accepted.',
  },
};

export const zPlanFile = {
  ...zAccountRef,
  root: z.string().min(1).optional(),
  staging: z.string().min(1).optional(),
  path: z.string().min(1).optional(),
  file_type: z.string().min(1).optional(),
  column_map: z.record(z.string(), z.unknown()).optional(),
  start: zDate.optional(),
  end: zDate.optional(),
  category_map: z.record(z.string(), zId).optional(),
  fallback_category_ids: z.object({ expense: zId.optional(), income: zId.optional() }).strict().optional(),
  accept_matches: z.array(z.object({ import_id: z.string().min(1).optional(), row_id: z.string().min(1).optional(), transaction_id: zId }).strict()).optional(),
};

export const planFileImport: ToolDef = {
  name: 'ezb_plan_file_import',
  route: { method: 'POST', path: '/ingest/file/plan' },
  tier: 'read',
  description: describe({
    what: "Previews the Import dialog for one file into one account: the rows the app's converter parsed, which are already in the books by the plane's import record, possible matches, unmapped categories, and the confirm_token for ezb_apply_file_import.",
    tier: 'read',
    insteadOf: 'For a whole archive use ezb_plan_statement_import. Then ezb_apply_file_import with the token.',
  }),
  inputSchema: objectSchema(PLAN_FILE_PROPERTIES, ['path']),
  schema: z.object({ ...zPlanFile, path: z.string().min(1) }).strict(),
  async run(args, ctx) {
    const res = await ctx.client.request('/ingest/file/plan', { method: 'POST', body: withRoot(args as RootArgs, ctx), noTimeout: true });
    return fromPlane(res, STATEMENT_UNTRUSTED);
  },
};

export const STATEMENT_TOOLS: ToolDef[] = [
  getStatementManifest,
  scanStatements,
  listMissingStatements,
  listStatementDuplicates,
  getStatementRows,
  describeStatementMap,
  extractStatements,
  planAccounts,
  planStatementImport,
  planFileImport,
  listImportFallout,
];

export const ACCOUNT_KINDS = KINDS;
