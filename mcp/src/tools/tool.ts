/**
 * The tool shape — pm/mcp.mdx §9.
 *
 * A tool validates its arguments, makes ONE machine-plane call, and returns the app's answer. It
 * does not sum, filter, sort, round, convert, join, cache, or decide anything two reasonable people
 * could disagree about (§9.4). If a tool seems to need computation, the computation becomes a
 * machine-plane route first and the tool calls it.
 */
import { z } from 'zod';

import type { MachinePlaneClient, PlaneResponse } from '../client.js';
import type { Config } from '../config.js';
import { fail } from '../envelope.js';
import type { ErrorCode } from '../envelope.js';

export type Tier = 'read' | 'write';

export type ToolContext = {
  client: MachinePlaneClient;
  config: Config;
};

export type ToolResult = {
  data: unknown;
  /** The plane's meta, passed through (user, defaultCurrency, timezone, truncated, partial …). */
  meta?: Record<string, unknown> | undefined;
  /** Field names carrying bank/statement/operator text (§7.5), when the plane did not say. */
  untrusted?: string[];
  truncated?: boolean;
  limitApplied?: number;
};

/**
 * The machine-plane route a tool needs (apis.mdx §10, §12, §14). `path` is the route PATTERN as
 * /capabilities publishes it (`/accounts/:id/balance`), not the concrete URL a call builds. Every
 * tool names exactly one, and a test asserts the mapping both ways against the plane's own table.
 */
export type RouteRef = {
  method: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE';
  path: string;
};

export type ToolDef = {
  name: string;
  route: RouteRef;
  /** Hand-authored JSON Schema — the model reads these words verbatim (§8.2). */
  inputSchema: Record<string, unknown>;
  description: string;
  tier: Tier;
  /** Runtime parse, gate 5. Unknown keys refused, limits clamped (§7.1). */
  schema: z.ZodType;
  /** Write tools only: whether the tool has a dry_run (every write but ezb_undo, §9.6). */
  hasDryRun?: boolean;
  /**
   * The route is admin tier on the plane (§9.5b). Only ADMIN_TOOLS may set it; the tool is then
   * gated by a third switch, EZBKMCP_ALLOW_ADMIN=1, on top of EZBKMCP_ALLOW_WRITE=1.
   */
  admin?: boolean;
  /** Exempt from the per-call timeout (§14). */
  noTimeout?: boolean;
  run(args: unknown, ctx: ToolContext): Promise<ToolResult>;
};

/** The closed verb set of §9.1. `add_ update_ set_ remove_ apply_` write; the rest read. */
export const READ_VERBS = ['list_', 'get_', 'count_', 'export_', 'convert_', 'scan_', 'extract_', 'plan_', 'describe_', 'find_'] as const;
/** `convert_to_` writes (ezb_convert_to_transfer) although `convert_` reads (ezb_convert_amount): the longest verb wins. `delete_` is ADMIN_TOOLS only. */
export const WRITE_VERBS = ['add_', 'update_', 'set_', 'remove_', 'apply_', 'convert_to_', 'delete_'] as const;
/** The one sanctioned admin-tier tool (§9.5b): the operator asked for a removal tool. */
export const ADMIN_TOOLS = ['ezb_delete_transactions'] as const;

/** The tier a tool's verb implies — the LONGEST matching verb decides — or undefined for a bare/analytics name. */
export function verbTier(name: string): 'read' | 'write' | undefined {
  const rest = name.startsWith('ezb_') ? name.slice('ezb_'.length) : name;
  let best: { len: number; tier: 'read' | 'write' } | undefined;
  for (const v of READ_VERBS) {
    if (rest.startsWith(v) && (best === undefined || v.length > best.len)) {
      best = { len: v.length, tier: 'read' };
    }
  }
  for (const v of WRITE_VERBS) {
    if (rest.startsWith(v) && (best === undefined || v.length > best.len)) {
      best = { len: v.length, tier: 'write' };
    }
  }
  return best?.tier;
}
/** Analytics tools named for the question, plus the three bare names. */
export const BARE_NAMES = ['ezb_whoami', 'ezb_health', 'ezb_capabilities', 'ezb_undo'] as const;

/**
 * The fourth mandatory description clause (§9.2), written out in full on every tool — because a
 * model that has read a description and still does not know whose tool it is will route on the
 * noun instead, and `actual_budget` has the same nouns.
 */
export const WHICH_SERVER =
  "This server is the operator's OWN ezBookkeeping install on this computer. " +
  'It is not Actual Budget (`actual_budget`), not company bookkeeping (`quickbooks`), and not a film project (`act3`).';

/** The second clause, for the tools that only read. */
export const READS_ONLY = 'Reads only.';

/** The second clause, for the tools that do not. */
export const WRITES = "WRITES to the operator's real ezBookkeeping books. Previews by default; applying needs a confirm token from the preview.";

/**
 * Assemble a description from its mandatory clauses. Going through one function is what lets a
 * test assert all four are present on every tool, rather than hoping nobody pasted one by hand.
 */
export function describe(opts: {
  /** Clause 1 — what it does, in domain language, one sentence. */
  what: string;
  /** Clause 2 — the cost. */
  tier: Tier;
  /** Clause 3 — which sibling to use instead, when there is one. */
  insteadOf?: string;
}): string {
  const parts = [opts.what, opts.tier === 'read' ? READS_ONLY : WRITES];
  if (opts.insteadOf !== undefined) {
    parts.push(opts.insteadOf);
  }
  parts.push(WHICH_SERVER);
  return parts.join(' ');
}

/**
 * The runtime half of the money rule (§10.1). A model asked for "about $123" produces 123.5, and
 * zod's own message says the type is wrong without saying what to send. This names the integer it
 * almost certainly meant, which turns a retry loop into one corrected call.
 */
export function hundredths() {
  return z.number().superRefine((value: number, ctx) => {
    if (Number.isInteger(value)) {
      return;
    }
    ctx.addIssue({
      code: 'custom',
      message: `amounts are integer HUNDREDTHS, never decimals — ${String(value)} is not an integer. Did you mean ${String(Math.round(value * 100))}?`,
    });
  });
}

/** An integer-hundredths JSON Schema field (§10.1): always "integer", always says hundredths, always an example. */
export function amountField(description: string): Record<string, unknown> {
  return {
    type: 'integer',
    description: `${description} In integer hundredths of the currency, never a decimal — 50000 = 500.00 in the account's currency, -12350 = -123.50.`,
  };
}

export function dateField(description: string): Record<string, unknown> {
  return { type: 'string', pattern: '^\\d{4}-\\d{2}-\\d{2}$', description: `${description} As YYYY-MM-DD in the resolved timezone.` };
}

export function idField(description: string): Record<string, unknown> {
  return { type: 'string', description: `${description} An ezBookkeeping id: a long decimal string exactly as a list tool returned it (never a UUID, never a number).` };
}

export function idListField(description: string): Record<string, unknown> {
  return { type: 'array', items: { type: 'string' }, description: `${description} ezBookkeeping ids as decimal strings, exactly as returned.` };
}

export function currencyField(description: string): Record<string, unknown> {
  return { type: 'string', pattern: '^[A-Z]{3}$', description: `${description} An ISO 4217 code such as USD, EUR or JPY.` };
}

export function boolField(description: string): Record<string, unknown> {
  return { type: 'boolean', description };
}

export function strField(description: string): Record<string, unknown> {
  return { type: 'string', description };
}

export function intField(description: string, extra: Record<string, unknown> = {}): Record<string, unknown> {
  return { type: 'integer', description, ...extra };
}

export function enumField(description: string, values: readonly string[]): Record<string, unknown> {
  return { type: 'string', enum: [...values], description: `${description} One of: ${values.join(', ')}.` };
}

/** The date-string and id-string zod shapes. */
export const zDate = z.string().regex(/^\d{4}-\d{2}-\d{2}$/, 'dates are YYYY-MM-DD');
export const zId = z.string().regex(/^\d+$/, 'ezBookkeeping ids are decimal strings, exactly as a list tool returned them');
export const zCurrency = z.string().regex(/^[A-Z]{3}$/, 'currencies are ISO 4217 codes such as USD');

/** Build a JSON-Schema object with the given properties; unknown keys are refused (§7.1 gate 5). */
export function objectSchema(properties: Record<string, unknown>, required: string[] = []): Record<string, unknown> {
  return {
    type: 'object',
    properties,
    ...(required.length > 0 ? { required } : {}),
    additionalProperties: false,
  };
}

// ─── the write protocol (§9.7, apis.mdx §9) ──────────────────────────────────────────────────

/** The three arguments every write tool but ezb_undo carries. `confirm` is an alias of `confirm_token`. */
export const WRITE_PROPERTIES: Record<string, unknown> = {
  dry_run: {
    type: 'boolean',
    description:
      'Preview only (the default, true). Nothing changes until the call is repeated with dry_run: false and the confirm_token the preview returned. Always show the operator the preview and wait for a real yes first.',
  },
  confirm_token: {
    type: 'string',
    description:
      'The confirm_token from the matching preview (this tool with dry_run: true, or the matching plan_ tool). Required with dry_run: false. It expires after ten minutes and fingerprints the exact change set; if the books moved since, the apply is refused with conflict and the new counts.',
  },
  confirm: { type: 'string', description: 'Alias of confirm_token.' },
  max_changes: {
    type: 'integer',
    minimum: 1,
    description: 'The ceiling on how many transactions may change (default 200). Over it the call refuses with too_many_changes and the real count; raise it deliberately, never by reflex.',
  },
};

export const zWrite = {
  dry_run: z.boolean().optional(),
  confirm_token: z.string().optional(),
  confirm: z.string().optional(),
  max_changes: z.number().int().positive().optional(),
};

export type WriteArgs = { dry_run?: boolean; confirm_token?: string; confirm?: string; max_changes?: number };

/** The plane's WriteOpts from the tool's arguments (dry_run defaults to true on both sides). */
export function writeOpts(args: WriteArgs, config: Config): { dry_run: boolean; confirm_token?: string; max_changes: number } {
  const token = args.confirm_token ?? args.confirm;
  return {
    dry_run: args.dry_run ?? true,
    ...(token === undefined ? {} : { confirm_token: token }),
    max_changes: args.max_changes ?? config.maxChanges,
  };
}

/** Strip the write-protocol keys off the args, leaving the route's own body. */
export function withoutWriteKeys<T extends Record<string, unknown>>(args: T): Omit<T, 'dry_run' | 'confirm_token' | 'confirm' | 'max_changes'> {
  const { dry_run: _d, confirm_token: _c, confirm: _c2, max_changes: _m, ...rest } = args;
  return rest;
}

/** Turn a plane response into the tool result, passing the plane's meta through verbatim. */
export function fromPlane(res: PlaneResponse, untrusted?: string[]): ToolResult {
  if (res.raw !== undefined) {
    return {
      data: { contentType: res.raw.contentType, fileName: res.raw.fileName ?? null, content: res.raw.text },
      meta: res.meta,
    };
  }
  return { data: res.data, meta: res.meta, ...(untrusted === undefined ? {} : { untrusted }) };
}

/** Shorthand for a failure a tool raises itself. */
export function toolFail(code: ErrorCode, message: string, hint?: string): never {
  throw fail(code, message, hint);
}

/** Clamp a requested row limit against the cap, reporting when it bound (§15): a silent cap is a wrong count. */
export function clampLimit(requested: number | undefined, config: Config): { limit: number; clamped: boolean } {
  if (requested === undefined) {
    return { limit: Math.min(200, config.maxRows), clamped: false };
  }
  if (requested > config.maxRows) {
    return { limit: config.maxRows, clamped: true };
  }
  return { limit: requested, clamped: false };
}

/** URL-encode one path segment (an id or a name). */
export function seg(v: string): string {
  return encodeURIComponent(v);
}
