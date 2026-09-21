/**
 * Orientation — pm/mcp.mdx §9.5. Four read tools: the first call of any session that will do anything.
 */
import { z } from 'zod';

import { CLIENT_VERSION } from '../client.js';

import { describe, fromPlane, objectSchema } from './tool.js';
import type { ToolDef } from './tool.js';

const NO_ARGS = objectSchema({});
const zNoArgs = z.object({}).strict();

export const whoami: ToolDef = {
  name: 'ezb_whoami',
  route: { method: 'GET', path: '/whoami' },
  tier: 'read',
  description: describe({
    what: 'Reports which install this is, the bound user, their default currency, the timezone, the server version, the key fingerprint, the write tier on both sides and the target — call it first whenever a number looks wrong or before any write.',
    tier: 'read',
    insteadOf: 'For whether the app is up at all and what to do if not, use ezb_health.',
  }),
  inputSchema: NO_ARGS,
  schema: zNoArgs,
  async run(_args, ctx) {
    const res = await ctx.client.request('/whoami');
    const data = res.data !== null && typeof res.data === 'object' ? (res.data as Record<string, unknown>) : { plane: res.data };
    return fromPlane({
      ...res,
      data: {
        ...data,
        mcp: {
          server: 'ezbookkeeping',
          version: CLIENT_VERSION,
          target: ctx.config.target,
          apiUrl: ctx.config.apiUrl,
          allowWrite: ctx.config.allowWrite,
          writeSwitches: {
            server: 'EZBK_MACHINE_ALLOW_WRITE=1 (ezbk up --allow-write)',
            mcp: 'EZBKMCP_ALLOW_WRITE=1',
          },
        },
      },
    });
  },
};

export const health: ToolDef = {
  name: 'ezb_health',
  route: { method: 'GET', path: '/health' },
  tier: 'read',
  description: describe({
    what: "Reports whether the app is up, whether a user is bound, whether upstream's full-access API-token and MCP doors are switched on, and what to do next; if the app is down the error names the exact command (`ezbk up`) — this server never starts it.",
    tier: 'read',
    insteadOf: 'For the bound user, currency and timezone, use ezb_whoami.',
  }),
  inputSchema: NO_ARGS,
  schema: zNoArgs,
  async run(_args, ctx) {
    const res = await ctx.client.request('/health');
    return fromPlane(res);
  },
};

export const capabilities: ToolDef = {
  name: 'ezb_capabilities',
  route: { method: 'GET', path: '/capabilities' },
  tier: 'read',
  description: describe({
    what: "Reports what this build of the app can do: its API version, tiers, which upstream features (import, export, pictures, scheduled transactions) are switched on in its .ini, the machine plane's route table with each route's status, and its limits.",
    tier: 'read',
    insteadOf: 'To learn whether a user is bound, use ezb_health.',
  }),
  inputSchema: NO_ARGS,
  schema: zNoArgs,
  async run(_args, ctx) {
    const res = await ctx.client.request('/capabilities');
    return fromPlane(res);
  },
};

export const getDataSummary: ToolDef = {
  name: 'ezb_get_data_summary',
  route: { method: 'GET', path: '/data/statistics' },
  tier: 'read',
  description: describe({
    what: 'Counts the books: how many accounts, transactions, categories, tags, templates, scheduled transactions, insights and pictures the bound user has — the size of the books, computed by the app.',
    tier: 'read',
    insteadOf: 'For how many transactions match a filter, use ezb_count_transactions.',
  }),
  inputSchema: NO_ARGS,
  schema: zNoArgs,
  async run(_args, ctx) {
    const res = await ctx.client.request('/data/statistics');
    return fromPlane(res);
  },
};

export const ORIENTATION_TOOLS: ToolDef[] = [whoami, health, capabilities, getDataSummary];
