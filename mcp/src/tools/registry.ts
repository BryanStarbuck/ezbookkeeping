/**
 * The ONE array — pm/mcp.mdx §9.3 (LOCKED).
 *
 * tools/list and dispatch both read TOOLS. A tool present in the catalogue but missing from
 * dispatch is a model calling it and getting -32601, which looks like a broken server; a single
 * array makes that impossible. Seventy-four tools in ten families: fifty read, twenty-four
 * write; one of the writes (ezb_delete_transactions, §9.5b) deletes, at the admin tier.
 */
import { ACCOUNT_TOOLS } from './accounts.js';
import { ANALYTICS_TOOLS } from './analytics.js';
import { CURRENCY_TOOLS } from './currency.js';
import { ORIENTATION_TOOLS } from './orientation.js';
import { PREVIEW_TOOLS } from './previews.js';
import { REFERENCE_TOOLS } from './reference.js';
import { STATEMENT_TOOLS } from './statements.js';
import type { ToolDef } from './tool.js';
import { TRANSACTION_TOOLS } from './transactions.js';
import { TRANSFER_TOOLS } from './transfers.js';
import { WRITE_TOOLS } from './writes.js';

export const FAMILIES: ReadonlyArray<{ name: string; tools: ToolDef[] }> = [
  { name: 'orientation', tools: ORIENTATION_TOOLS },
  { name: 'accounts', tools: ACCOUNT_TOOLS },
  { name: 'transactions', tools: TRANSACTION_TOOLS },
  { name: 'reference', tools: REFERENCE_TOOLS },
  { name: 'currency', tools: CURRENCY_TOOLS },
  { name: 'analytics', tools: ANALYTICS_TOOLS },
  { name: 'statements', tools: STATEMENT_TOOLS },
  { name: 'previews', tools: PREVIEW_TOOLS },
  { name: 'writes', tools: WRITE_TOOLS },
  { name: 'transfers', tools: TRANSFER_TOOLS },
];

export const TOOLS: readonly ToolDef[] = Object.freeze(FAMILIES.flatMap(f => f.tools));

const BY_NAME: ReadonlyMap<string, ToolDef> = new Map(TOOLS.map(t => [t.name, t]));

export function findTool(name: string): ToolDef | undefined {
  return BY_NAME.get(name);
}

export const TOTAL_TOOLS = TOOLS.length;
export const READ_TOOLS = TOOLS.filter(t => t.tier === 'read').length;
export const WRITE_TOOL_COUNT = TOOLS.filter(t => t.tier === 'write').length;
