/**
 * The six gates — pm/mcp.mdx §7.1.
 *
 *   1. Transport — stdio only; this process binds no port (index.ts).
 *   2. Key — read at startup from a 0600, correctly-owned file, or the server did not start (index.ts).
 *   3. Target — the base URL is loopback, or the §7.3 tripwire was armed deliberately (config.ts).
 *   4. Mode — read tools always; write tools only with EZBKMCP_ALLOW_WRITE=1 (and the server's tier);
 *      the one admin tool (ezb_delete_transactions) also needs EZBKMCP_ALLOW_ADMIN=1.
 *   5. Input — the tool's Zod schema; unknown keys refused, limits clamped, decimals named.
 *   6. The machine plane's own ladder, which trusts none of 1–5.
 *
 * What lives here is the per-call half: the routing check (§3.4 layer 6), gate 4, gate 5, and the
 * confirm check that mints `confirm_required` before a write with no token ever leaves this process.
 */
import type { Config } from './config.js';
import { fail } from './envelope.js';
import type { ToolDef } from './tools/tool.js';

/**
 * Gate 4 — mode. A write tool the mode gate rejects is still LISTED in tools/list, with its
 * description saying it is disabled and how to enable it. A tool that vanishes teaches the model
 * nothing, and a model that cannot see the tool will look for another way to do the same thing.
 */
export function checkMode(tool: ToolDef, config: Config): void {
  if (tool.tier !== 'write') {
    return;
  }

  if (config.target === 'remote') {
    throw fail(
      'write_disabled',
      'This server is pointed at a remote install, which is read-only.',
      'a remote target never permits writes, with or without the write switches',
    );
  }

  if (!config.allowWrite) {
    // Both switches named, because the operator has to set both (§9.7).
    throw fail(
      'write_disabled',
      'The write tier is off.',
      'Restart the app with `ezbk stop && ezbk up --allow-write` (EZBK_MACHINE_ALLOW_WRITE=1) and set EZBKMCP_ALLOW_WRITE=1 for this server, then retry.',
    );
  }

  if (tool.admin === true && !config.allowAdmin) {
    // The one admin-tier tool (§9.5b) needs a third switch here and the admin tier on the app.
    throw fail(
      'write_disabled',
      `${tool.name} deletes, and the admin tier is off.`,
      'Restart the app with `ezbk stop && ezbk up --allow-write --allow-admin` (EZBK_MACHINE_ALLOW_ADMIN=1) and set EZBKMCP_ALLOW_ADMIN=1 (with EZBKMCP_ALLOW_WRITE=1) for this server, then retry.',
    );
  }
}

/**
 * Gate 5 — input. Unknown keys are refused by the strict schemas; limits are clamped inside the
 * tools; a decimal amount is rejected naming the integer it probably meant (tool.ts hundredths()).
 */
export function checkInput<T>(tool: ToolDef, args: unknown): T {
  const parsed = tool.schema.safeParse(args ?? {});
  if (!parsed.success) {
    const issue = parsed.error.issues[0];
    const where = issue?.path.map(String).join('.') ?? '';
    const detail = issue?.message ?? 'invalid arguments';
    throw fail(
      'invalid_input',
      where === '' ? detail : `${where}: ${detail}`,
      "check the tool's input schema and retry with corrected arguments; unknown keys are refused, ids are strings, amounts are integer hundredths",
    );
  }
  return parsed.data as T;
}

/**
 * The confirm protocol's client half (§9.7). A write with dry_run: false and no token is refused
 * HERE as `confirm_required`, naming where the token comes from, before any request is made. The
 * plane enforces its own half regardless.
 */
export function checkConfirm(tool: ToolDef, args: unknown): void {
  if (tool.tier !== 'write' || tool.hasDryRun !== true) {
    return;
  }
  const a = args as { dry_run?: boolean; confirm_token?: string; confirm?: string };
  if (a.dry_run !== false) {
    return;
  }
  const token = a.confirm_token ?? a.confirm;
  if (token === undefined || token.trim() === '') {
    const source = tool.name.startsWith('ezb_apply_') ? `the matching plan_ tool (${planToolFor(tool.name)})` : `${tool.name} with dry_run: true`;
    throw fail(
      'confirm_required',
      `${tool.name} with dry_run: false needs the confirm_token from its preview.`,
      `call ${source} first, show the operator the preview, and pass its confirm_token here`,
    );
  }
}

function planToolFor(applyName: string): string {
  switch (applyName) {
    case 'ezb_apply_accounts':
      return 'ezb_plan_accounts';
    case 'ezb_apply_statement_import':
      return 'ezb_plan_statement_import';
    case 'ezb_apply_file_import':
      return 'ezb_plan_file_import';
    case 'ezb_apply_reconcile':
      return 'ezb_plan_reconcile';
    default:
      return applyName.replace('ezb_apply_', 'ezb_plan_');
  }
}

/**
 * Layer 6 of §3.4 — refuse-with-redirect on a foreign-shaped argument. This catches the model that
 * has ALREADY routed wrong and teaches it at the moment it crosses. The redirect names a server; it
 * is the only place this server emits text telling a model to call something else (§7.5).
 *
 * ezBookkeeping ids are 18–19 digit decimal strings. Actual Budget's are UUIDs. Only id-shaped
 * argument names are inspected, so a UUID in an idempotency_key or a free-text keyword is not a
 * false positive.
 */
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const ACT3_ID = /^(proj|project|shot|scene|take|render)[-_][A-Za-z0-9]/i;
const QB_DOC = /^(INV|BILL|EST|PO)-?\d+$/i;
const YEAR_MONTH = /^\d{4}-\d{2}$/;

const ACTUAL_BUDGET_KEYS = new Set(['budget_month', 'budget_id', 'budget', 'to_budget', 'carryover', 'cover_from', 'cover_overspending', 'payee_id', 'payee_ids', 'schedule_id', 'rule_id']);
const QUICKBOOKS_KEYS = new Set(['realm_id', 'realmid', 'invoice_id', 'invoice_number', 'bill_id', 'vendor_id', 'customer_id', 'journal_entry_id']);
const ACT3_KEYS = new Set(['project_id', 'shot_id', 'scene_id', 'take_id', 'render_id']);

function isIdKey(key: string): boolean {
  return key === 'id' || key === 'ids' || key.endsWith('_id') || key.endsWith('_ids');
}

function redirect(looksLike: string, server: string, tools: string): never {
  throw fail(
    'wrong_server',
    `This looks like ${looksLike}. This server is ezBookkeeping — the operator's own books in the ezBookkeeping install on this computer.`,
    `Use the \`${server}\` server's ${tools}.`,
  );
}

export function checkNotForeign(args: unknown, depth = 0): void {
  if (args === null || typeof args !== 'object' || depth > 3) {
    return;
  }
  for (const [key, value] of Object.entries(args as Record<string, unknown>)) {
    const k = key.toLowerCase();

    if (ACTUAL_BUDGET_KEYS.has(k)) {
      redirect(`an Actual Budget argument (${key}); ezBookkeeping has no envelope budget, budget months, To Budget or carryover`, 'actual_budget', '`ab_` tools');
    }
    if (QUICKBOOKS_KEYS.has(k)) {
      redirect(`a QuickBooks argument (${key})`, 'quickbooks', 'tools');
    }
    if (ACT3_KEYS.has(k)) {
      redirect(`an ACT3 argument (${key})`, 'act3', 'tools');
    }

    if (typeof value === 'string') {
      if (isIdKey(k)) {
        if (UUID.test(value)) {
          redirect('an Actual Budget id (a UUID; ezBookkeeping ids are 18–19 digit decimal strings)', 'actual_budget', '`ab_` tools');
        }
        if (ACT3_ID.test(value)) {
          redirect('an ACT3 project identifier', 'act3', 'tools');
        }
        if (QB_DOC.test(value)) {
          redirect('a QuickBooks invoice, bill, estimate or purchase-order number', 'quickbooks', 'tools');
        }
      }
      if (k === 'month' && YEAR_MONTH.test(value)) {
        redirect('a budget month (YYYY-MM); ezBookkeeping has no budget months — use start and end dates here', 'actual_budget', '`ab_` tools');
      }
    } else if (Array.isArray(value)) {
      if (isIdKey(k)) {
        for (const item of value) {
          if (typeof item === 'string' && UUID.test(item)) {
            redirect('an Actual Budget id (a UUID; ezBookkeeping ids are 18–19 digit decimal strings)', 'actual_budget', '`ab_` tools');
          }
        }
      }
      for (const item of value) {
        checkNotForeign(item, depth + 1);
      }
    } else if (value !== null && typeof value === 'object') {
      checkNotForeign(value, depth + 1);
    }
  }
}
