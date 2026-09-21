/**
 * Previews and history — pm/mcp.mdx §9.5. Two read tools: the reconciliation plan, and the journal
 * of machine-plane writes that ezb_undo reverses.
 */
import { z } from 'zod';

import { ACCOUNT_REF_PROPERTIES, accountSegment, zAccountRef } from './accounts.js';
import { amountField, boolField, clampLimit, dateField, describe, fromPlane, hundredths, idField, intField, objectSchema, strField, zDate, zId } from './tool.js';
import type { ToolDef, ToolResult } from './tool.js';

export const planReconcile: ToolDef = {
  name: 'ezb_plan_reconcile',
  route: { method: 'POST', path: '/accounts/:id/reconcile/plan' },
  tier: 'read',
  description: describe({
    what: "Compares the app's balance for one account as of a date against the figure a printed statement shows, and returns the signed difference, the transactions dated after the last reconciliation (where the missing one usually is), and the confirm_token for ezb_apply_reconcile; if the difference is not zero, hunt it before marking anything reconciled.",
    tier: 'read',
    insteadOf: "For the app's own statement of the period use ezb_get_reconciliation_statement.",
  }),
  inputSchema: objectSchema(
    {
      ...ACCOUNT_REF_PROPERTIES,
      target_balance: amountField("The balance the operator's statement shows."),
      as_of: dateField('The date the statement balance is for. Defaults to today.'),
      create_adjustment: boolField('Plan one visible adjustment transaction for the difference. Defaults to false: find the missing transaction instead. The apply must pass the same arguments.'),
      adjustment_category_id: idField('The secondary category the adjustment would go in.'),
      adjustment_category_name: strField('The adjustment category by name.'),
      adjustment_comment: strField('The comment the adjustment would carry.'),
      mark_reconciled: boolField("Plan to set the account's last reconciled time. Defaults to true; pass false when the bound user has last-reconciled-time switched off."),
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
    })
    .strict(),
  async run(args, ctx) {
    const a = args as { account_id?: string; account_name?: string } & Record<string, unknown>;
    const { account_id: _id, account_name: _n, ...body } = a;
    const res = await ctx.client.request(`/accounts/${accountSegment(a)}/reconcile/plan`, { method: 'POST', body });
    return fromPlane(res, ['comment', 'name']);
  },
};

export const listJournal: ToolDef = {
  name: 'ezb_list_journal',
  route: { method: 'GET', path: '/journal' },
  tier: 'read',
  description: describe({
    what: 'Lists recent machine-plane writes made through this server or the CLI — route, when, change counts, client, whether undone — which is what ezb_undo would reverse next; never amounts.',
    tier: 'read',
    insteadOf: 'To reverse the latest one, use ezb_undo.',
  }),
  inputSchema: objectSchema({
    limit: intField('Entries to return; clamped to this server\'s cap.', { minimum: 1 }),
    offset: intField('Entries to skip.', { minimum: 0 }),
    include_undone: boolField('Include entries that were already undone. Defaults to false.'),
  }),
  schema: z.object({ limit: z.number().int().positive().optional(), offset: z.number().int().min(0).optional(), include_undone: z.boolean().optional() }).strict(),
  async run(args, ctx) {
    const a = args as { limit?: number; offset?: number; include_undone?: boolean };
    const { limit, clamped } = clampLimit(a.limit, ctx.config);
    const res = await ctx.client.request('/journal', { query: { limit, offset: a.offset, include_undone: a.include_undone } });
    const out: ToolResult = fromPlane(res, ['summary']);
    if (clamped) {
      out.truncated = true;
      out.limitApplied = limit;
    }
    return out;
  },
};

export const PREVIEW_TOOLS: ToolDef[] = [planReconcile, listJournal];
