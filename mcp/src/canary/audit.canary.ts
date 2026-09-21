/**
 * T11 — PII in the log (pm/mcp.mdx §7.0, §16.2, AC 16): arguments hashed, the allowlist verbatim,
 * never an amount, comment, account id, keyword or path; the key only as a fingerprint; a denial
 * logs the gate and the code, never the value.
 */
import { describe, expect, it } from 'vitest';

import { auditLine, hashArgs, LOGGABLE_SCALARS } from '../audit.js';

describe('canary: audit', () => {
  const args = {
    interval: 'month',
    group_by: 'primary',
    limit: 20,
    include_hidden: true,
    include_transfers: false,
    dry_run: true,
    convert_to: 'USD',
    currency: 'EUR',
    type: 'expense',
    kind: 'scheduled',
    format: 'csv',
    days: 30,
    // never logged:
    account_ids: ['3401855937219633152'],
    keyword: 'NORTHSIDE MARKET',
    comment: 'ACME HARDWARE',
    min_amount: 123456,
    amount: 98765,
    root: '/home/somebody/statements',
    path: 'bank/household/Northbank/2026-08.ofx',
    name: 'Household · Northbank Checking ••4021',
    transactions: [{ amount: 55555, comment: 'LISBON BISTRO' }],
  };

  it('logs exactly the allowlist of §16.2', () => {
    expect([...LOGGABLE_SCALARS].sort()).toEqual(['convert_to', 'currency', 'days', 'dry_run', 'format', 'group_by', 'include_hidden', 'include_transfers', 'interval', 'kind', 'limit', 'type']);
  });

  it('a CALL line carries the hash, the allowlisted scalars, the count and the fingerprint — nothing else', () => {
    const line = auditLine({ tool: 'ezb_net_worth', tier: 'read', target: 'local', user: 'operator', args, ok: true, tookMs: 38, keyFingerprint: 'a3f1…/sha256:9c2b', rows: 12 });
    expect(line).toMatch(/^\d{4}-\d{2}-\d{2}T\S+Z CALL ezb_net_worth tier=read target=local user=operator args=sha256:[0-9a-f]{8} /);
    for (const k of LOGGABLE_SCALARS) {
      expect(line).toContain(`${k}=`);
    }
    expect(line).toContain('rows=12 ok=true tookMs=38 key=a3f1…/sha256:9c2b');
    for (const forbidden of ['3401855937219633152', 'NORTHSIDE', 'ACME', '123456', '98765', '55555', '/home/', 'Northbank', '4021', 'LISBON', 'household']) {
      expect(line).not.toContain(forbidden);
    }
  });

  it('a denial logs the gate and the code, never the value that failed', () => {
    const line = auditLine({ tool: 'ezb_get_account', tier: 'read', target: 'local', args: { account_id: '11111111-2222-4333-8444-555555555555' }, ok: false, tookMs: 1, keyFingerprint: 'a3f1…/sha256:9c2b', gate: 'routing', errorCode: 'wrong_server' });
    expect(line).toContain('ok=false gate=routing code=wrong_server');
    expect(line).not.toContain('11111111');
  });

  it('the hash is stable for equal arguments and differs otherwise', () => {
    expect(hashArgs({ a: 1 })).toBe(hashArgs({ a: 1 }));
    expect(hashArgs({ a: 1 })).not.toBe(hashArgs({ a: 2 }));
    expect(hashArgs(undefined)).toBe(hashArgs({}));
  });
});
