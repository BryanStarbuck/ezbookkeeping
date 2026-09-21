/**
 * The audit line — pm/mcp.mdx §16.2.
 *
 * One line per call, allowed or denied. The PII rules are absolute, because a log file is the
 * least-protected copy of anything it holds, and this one is about somebody's money.
 *
 *   - Arguments are HASHED, not logged. Only a non-PII scalar allowlist is logged verbatim.
 *   - No amount, ever. Counts yes, money no.
 *   - The key appears only as a fingerprint.
 *   - A denial logs the GATE that denied it and the error code, never the value that failed.
 *
 *   2026-09-21T17:41:02Z CALL ezb_net_worth tier=read target=local user=operator
 *     args=sha256:4f2a… interval=month convert_to=USD rows=12 ok=true tookMs=38 key=a3f1…/sha256:9c2b
 */
import crypto from 'node:crypto';

/**
 * The allowlist (§16.2). Everything not named here is hashed away. Note what is absent and why: an
 * account id identifies one of the operator's accounts, a keyword or comment names a merchant, an
 * amount is money, a path discloses their directory layout. The rest are knobs.
 */
export const LOGGABLE_SCALARS: ReadonlySet<string> = new Set([
  'interval',
  'group_by',
  'limit',
  'include_hidden',
  'include_transfers',
  'dry_run',
  'convert_to',
  'currency',
  'type',
  'kind',
  'format',
  'days',
]);

export function hashArgs(args: unknown): string {
  const json = JSON.stringify(args ?? {});
  return `sha256:${crypto.createHash('sha256').update(json).digest('hex').slice(0, 8)}`;
}

function allowlisted(args: unknown): string {
  if (args === null || typeof args !== 'object' || Array.isArray(args)) {
    return '';
  }
  const parts: string[] = [];
  for (const [key, value] of Object.entries(args as Record<string, unknown>)) {
    if (!LOGGABLE_SCALARS.has(key)) {
      continue;
    }
    if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
      parts.push(`${key}=${String(value)}`);
    }
  }
  return parts.join(' ');
}

export type AuditRecord = {
  tool: string;
  tier: 'read' | 'write';
  target: 'local' | 'remote';
  user?: string | undefined;
  args: unknown;
  ok: boolean;
  tookMs: number;
  keyFingerprint: string;
  /** Row count, when there is one. A count is safe; the rows are not. */
  rows?: number | undefined;
  /** On a denial: which gate, and the code. Never the value that failed. */
  gate?: string | undefined;
  errorCode?: string | undefined;
};

export function auditLine(record: AuditRecord): string {
  const parts = [new Date().toISOString(), 'CALL', record.tool, `tier=${record.tier}`, `target=${record.target}`];
  if (record.user !== undefined && record.user !== '') {
    parts.push(`user=${record.user}`);
  }
  parts.push(`args=${hashArgs(record.args)}`);

  const scalars = allowlisted(record.args);
  if (scalars !== '') {
    parts.push(scalars);
  }
  if (record.rows !== undefined) {
    parts.push(`rows=${String(record.rows)}`);
  }
  parts.push(`ok=${String(record.ok)}`);
  if (record.gate !== undefined) {
    parts.push(`gate=${record.gate}`);
  }
  if (record.errorCode !== undefined) {
    parts.push(`code=${record.errorCode}`);
  }
  parts.push(`tookMs=${String(record.tookMs)}`, `key=${record.keyFingerprint}`);

  return parts.join(' ');
}
