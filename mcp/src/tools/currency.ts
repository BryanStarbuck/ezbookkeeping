/**
 * Currency — pm/mcp.mdx §9.5, §10.2. Two read tools: the app's rates, and the app's conversion.
 * A model never multiplies by a rate itself.
 */
import { z } from 'zod';

import { amountField, currencyField, describe, fromPlane, hundredths, objectSchema, zCurrency } from './tool.js';
import type { ToolDef } from './tool.js';

export const getExchangeRates: ToolDef = {
  name: 'ezb_get_exchange_rates',
  route: { method: 'GET', path: '/exchange-rates' },
  tier: 'read',
  description: describe({
    what: "Gets the app's latest exchange rates — the provider's and the operator's custom ones, each marked with its source and update time — as units per one unit of the base currency; the app keeps latest rates only, never historical ones.",
    tier: 'read',
    insteadOf: 'To convert an amount, use ezb_convert_amount or convert_to on an analytics tool rather than multiplying yourself.',
  }),
  inputSchema: objectSchema({
    currencies: { type: 'array', items: { type: 'string', pattern: '^[A-Z]{3}$' }, description: 'Only these ISO 4217 currencies. Defaults to all the app has.' },
  }),
  schema: z.object({ currencies: z.array(zCurrency).optional() }).strict(),
  async run(args, ctx) {
    const a = args as { currencies?: string[] };
    const res = await ctx.client.request('/exchange-rates', { query: { currencies: a.currencies } });
    return fromPlane(res);
  },
};

export const convertAmount: ToolDef = {
  name: 'ezb_convert_amount',
  route: { method: 'POST', path: '/exchange-rates/convert' },
  tier: 'read',
  description: describe({
    what: "Converts one integer-hundredths amount from one currency to another with the app's own rates, returning the converted amount, the rate used, its source (provider or custom) and its update time, rounded once by the app.",
    tier: 'read',
    insteadOf: 'For a converted total over many transactions, pass convert_to to the analytics tool instead of converting rows one by one.',
  }),
  inputSchema: objectSchema(
    {
      amount: amountField('The amount to convert.'),
      from: currencyField('The currency the amount is in.'),
      to: currencyField('The currency to convert into.'),
    },
    ['amount', 'from', 'to'],
  ),
  schema: z.object({ amount: hundredths(), from: zCurrency, to: zCurrency }).strict(),
  async run(args, ctx) {
    const a = args as { amount: number; from: string; to: string };
    const res = await ctx.client.request('/exchange-rates/convert', { method: 'POST', body: { amount: a.amount, from: a.from, to: a.to } });
    return fromPlane(res);
  },
};

export const CURRENCY_TOOLS: ToolDef[] = [getExchangeRates, convertAmount];
