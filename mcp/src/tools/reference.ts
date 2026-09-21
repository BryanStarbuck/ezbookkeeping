/**
 * Reference data — pm/mcp.mdx §9.5. Seven read tools: categories (the list and the YAML tree), tags,
 * tag groups, templates and scheduled transactions, what the schedules will create, saved insights.
 */
import { z } from 'zod';

import { boolField, clampLimit, dateField, describe, enumField, fromPlane, idField, intField, objectSchema, strField, zDate, zId } from './tool.js';
import type { ToolDef, ToolResult } from './tool.js';

export const CATEGORY_TYPES = ['income', 'expense', 'transfer'] as const;
export const TEMPLATE_KINDS = ['normal', 'scheduled', 'all'] as const;

export const listCategories: ToolDef = {
  name: 'ezb_list_categories',
  route: { method: 'GET', path: '/categories' },
  tier: 'read',
  description: describe({
    what: 'Lists the two-level category tree — primary categories with their secondary categories nested — by type; transactions always use a SECONDARY category, so pick from the nested ones when writing.',
    tier: 'read',
    insteadOf: 'For spending per category, use ezb_spending_by_category rather than walking transactions.',
  }),
  inputSchema: objectSchema({
    type: enumField('Only categories of this type.', CATEGORY_TYPES),
    include_hidden: boolField('Include hidden categories. Defaults to false.'),
    parent_id: idField('Only the secondary categories under this primary category, as flat rows.'),
    name: strField('Only the category with this name (exact first, then case-insensitive), as flat rows.'),
    flat: boolField('Return flat rows instead of the nested tree.'),
  }),
  schema: z
    .object({
      type: z.enum(CATEGORY_TYPES).optional(),
      include_hidden: z.boolean().optional(),
      parent_id: zId.optional(),
      name: z.string().min(1).optional(),
      flat: z.boolean().optional(),
    })
    .strict(),
  async run(args, ctx) {
    const a = args as { type?: string; include_hidden?: boolean; parent_id?: string; name?: string; flat?: boolean };
    const res = await ctx.client.request('/categories', { query: { type: a.type, include_hidden: a.include_hidden, parent_id: a.parent_id, name: a.name, flat: a.flat } });
    return fromPlane(res, ['name', 'comment']);
  },
};

export const getCategoryTree: ToolDef = {
  name: 'ezb_get_category_tree',
  route: { method: 'GET', path: '/categories/tree' },
  tier: 'read',
  description: describe({
    what: 'Returns the whole category tree as ONE YAML document — every group (primary category) with its sub-categories, by major category (income, expense, transfer) — so a categoriser reads every choice before picking one; a transaction holds a sub-category, named by the path "Type > Group > Sub".',
    tier: 'read',
    insteadOf: 'Read this before categorising anything (ezb_set_transaction_categories, ezb_set_import_categories) and pick only paths it lists; create a missing path deliberately with ezb_add_categories. For one category or a filtered flat list, ezb_list_categories.',
  }),
  inputSchema: objectSchema({
    type: enumField('Only groups of this major category.', CATEGORY_TYPES),
    include_hidden: boolField('Include hidden groups and sub-categories. Defaults to false.'),
    format: enumField('yaml (the default): data.yaml is the document, with the counts. json: the structured groups as well.', ['yaml', 'json'] as const),
  }),
  schema: z
    .object({
      type: z.enum(CATEGORY_TYPES).optional(),
      include_hidden: z.boolean().optional(),
      format: z.enum(['yaml', 'json']).optional(),
    })
    .strict(),
  async run(args, ctx) {
    const a = args as { type?: string; include_hidden?: boolean; format?: 'yaml' | 'json' };
    const res = await ctx.client.request('/categories/tree', { query: { type: a.type, include_hidden: a.include_hidden } });
    if (a.format !== 'json' && res.data !== null && typeof res.data === 'object') {
      const d = res.data as Record<string, unknown>;
      return { data: { app: d.app, generatedAt: d.generatedAt, counts: d.counts, yaml: d.yaml, note: d.note }, meta: res.meta, untrusted: ['yaml'] };
    }
    return fromPlane(res, ['name', 'yaml']);
  },
};

export const listTags: ToolDef = {
  name: 'ezb_list_tags',
  route: { method: 'GET', path: '/tags' },
  tier: 'read',
  description: describe({
    what: 'Lists the tags — how the operator marks a trip, a project or a reimbursable — with their group and how many transactions carry each.',
    tier: 'read',
    insteadOf: 'For what a tag cost, use ezb_tag_breakdown.',
  }),
  inputSchema: objectSchema({
    include_hidden: boolField('Include hidden tags. Defaults to false.'),
    group_id: idField('Only tags in this tag group; "0" means ungrouped.'),
    group_name: strField('Only tags in the group with this name.'),
    name: strField('Only the tag with this name.'),
  }),
  schema: z.object({ include_hidden: z.boolean().optional(), group_id: zId.optional(), group_name: z.string().min(1).optional(), name: z.string().min(1).optional() }).strict(),
  async run(args, ctx) {
    const a = args as { include_hidden?: boolean; group_id?: string; group_name?: string; name?: string };
    const res = await ctx.client.request('/tags', { query: { include_hidden: a.include_hidden, group_id: a.group_id, group_name: a.group_name, name: a.name } });
    return fromPlane(res, ['name']);
  },
};

export const listTagGroups: ToolDef = {
  name: 'ezb_list_tag_groups',
  route: { method: 'GET', path: '/tag-groups' },
  tier: 'read',
  description: describe({
    what: 'Lists the tag groups with how many tags each holds.',
    tier: 'read',
    insteadOf: 'For the tags themselves, use ezb_list_tags.',
  }),
  inputSchema: objectSchema({ name: strField('Only the group with this name.') }),
  schema: z.object({ name: z.string().min(1).optional() }).strict(),
  async run(args, ctx) {
    const a = args as { name?: string };
    const res = await ctx.client.request('/tag-groups', { query: { name: a.name } });
    return fromPlane(res, ['name']);
  },
};

export const listTemplates: ToolDef = {
  name: 'ezb_list_templates',
  route: { method: 'GET', path: '/templates' },
  tier: 'read',
  description: describe({
    what: 'Lists transaction templates and scheduled transactions (kind scheduled: the rent on the 1st), each with its type, account, integer-hundredths amount, category, frequency and next occurrence.',
    tier: 'read',
    insteadOf: 'For what the schedules will actually create in the coming days, use ezb_list_upcoming_schedules; for repeating charges the operator has NOT scheduled, ezb_list_recurring.',
  }),
  inputSchema: objectSchema({
    kind: enumField('Which kind to list. Defaults to all.', TEMPLATE_KINDS),
    include_hidden: boolField('Include hidden templates. Defaults to false.'),
    name: strField('Only the template with this name.'),
  }),
  schema: z.object({ kind: z.enum(TEMPLATE_KINDS).optional(), include_hidden: z.boolean().optional(), name: z.string().min(1).optional() }).strict(),
  async run(args, ctx) {
    const a = args as { kind?: string; include_hidden?: boolean; name?: string };
    const res = await ctx.client.request('/templates', { query: { kind: a.kind, include_hidden: a.include_hidden, name: a.name } });
    return fromPlane(res, ['name', 'comment']);
  },
};

export const listUpcomingSchedules: ToolDef = {
  name: 'ezb_list_upcoming_schedules',
  route: { method: 'GET', path: '/schedules/upcoming' },
  tier: 'read',
  description: describe({
    what: "Lists what the scheduled transactions will create in the next N days, date by date, computed with the app's own cron calendar rule without firing anything.",
    tier: 'read',
    insteadOf: 'For the schedules as records, use ezb_list_templates with kind scheduled.',
  }),
  inputSchema: objectSchema({
    days: intField('How many days ahead to look. Defaults to 30.', { minimum: 1, maximum: 3660 }),
    from: dateField('The first day of the window. Defaults to today.'),
    template_ids: { type: 'array', items: { type: 'string' }, description: 'Only these schedules (ids or names).' },
    limit: intField('Occurrences to return; clamped to this server\'s cap.', { minimum: 1 }),
  }),
  schema: z.object({ days: z.number().int().min(1).max(3660).optional(), from: zDate.optional(), template_ids: z.array(z.string().min(1)).optional(), limit: z.number().int().positive().optional() }).strict(),
  async run(args, ctx) {
    const a = args as { days?: number; from?: string; template_ids?: string[]; limit?: number };
    const { limit, clamped } = clampLimit(a.limit, ctx.config);
    const res = await ctx.client.request('/schedules/upcoming', { query: { days: a.days, from: a.from, template_ids: a.template_ids, limit } });
    const out: ToolResult = fromPlane(res, ['name', 'comment']);
    if (clamped) {
      out.truncated = true;
      out.limitApplied = limit;
    }
    return out;
  },
};

export const listSavedInsights: ToolDef = {
  name: 'ezb_list_saved_insights',
  route: { method: 'GET', path: '/insights' },
  tier: 'read',
  description: describe({
    what: "Lists the operator's saved Insights Explorer definitions — the saved queries, not their numbers.",
    tier: 'read',
    insteadOf: 'For the numbers a saved insight would show, use the analytics tools (ezb_spending_by_category and friends).',
  }),
  inputSchema: objectSchema({
    include_hidden: boolField('Include hidden insights. Defaults to false.'),
    name: strField('Only the insight with this name.'),
  }),
  schema: z.object({ include_hidden: z.boolean().optional(), name: z.string().min(1).optional() }).strict(),
  async run(args, ctx) {
    const a = args as { include_hidden?: boolean; name?: string };
    const res = await ctx.client.request('/insights', { query: { include_hidden: a.include_hidden, name: a.name } });
    return fromPlane(res, ['name']);
  },
};

export const REFERENCE_TOOLS: ToolDef[] = [listCategories, getCategoryTree, listTags, listTagGroups, listTemplates, listUpcomingSchedules, listSavedInsights];
