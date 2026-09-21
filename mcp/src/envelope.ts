/**
 * The response contract — pm/mcp.mdx §12.
 *
 * One shape, every time. The model never has to guess whether it is parsing.
 */

/**
 * The closed vocabulary — twelve codes (§12.3).
 *
 * The first nine are the machine plane's (apis.mdx §7.2) and arrive on the wire. The last three
 * are minted HERE and never appear in an HTTP response: they name conditions this server detects
 * before or instead of a call. Every machine-plane code is also an MCP code, so a plane error
 * passes through unchanged, and a test asserts this list is a strict superset of the plane's.
 * These are deliberately the same twelve the `actual_budget` server uses.
 */
export const ERROR_CODES = [
  // From the machine plane.
  'unauthorized',
  'forbidden',
  'not_found',
  'invalid_input',
  'conflict',
  'write_disabled',
  'not_ready',
  'upstream_error',
  'internal',
  // Minted here only.
  'confirm_required',
  'too_many_changes',
  'wrong_server',
] as const;

export type ErrorCode = (typeof ERROR_CODES)[number];

/** The nine the plane answers with. Kept explicit so the parity test can assert the superset. */
export const PLANE_CODES: readonly ErrorCode[] = [
  'unauthorized',
  'forbidden',
  'not_found',
  'invalid_input',
  'conflict',
  'write_disabled',
  'not_ready',
  'upstream_error',
  'internal',
];

/** The three this server mints. */
export const MCP_ONLY_CODES: readonly ErrorCode[] = ['confirm_required', 'too_many_changes', 'wrong_server'];

/** Where an `internal` failure's detail lands (pm/error_err.mdx §3.1). */
export const ERROR_FILE_HINT = 'read ~/T/ezbookkeeping/error.err for the detail';

export type Meta = {
  /** Always "ezbookkeeping" — the cheapest defence against T3 (§12.1). */
  app: 'ezbookkeeping';
  user?: string;
  defaultCurrency?: string;
  target: 'local' | 'remote';
  serverVersion?: string;
  asOf: string;
  timezone?: string;
  tookMs: number;
  truncated: boolean;
  limitApplied?: number;
  /** Field names whose contents came from a bank, a statement or the operator (§7.5). */
  untrusted?: string[];
  [extra: string]: unknown;
};

export type SuccessEnvelope = {
  ok: true;
  tool: string;
  data: unknown;
  meta: Meta;
};

export type FailureEnvelope = {
  ok: false;
  tool: string;
  error: { code: ErrorCode; message: string; hint?: string; details?: unknown };
  meta?: Partial<Meta>;
};

export type Envelope = SuccessEnvelope | FailureEnvelope;

/**
 * A tool failure.
 *
 * Thrown by gates and handlers and turned into `isError: true` content by the host. It is NEVER a
 * JSON-RPC protocol error: losing one call must not kill a loop, and `-32601`/`-32603` are
 * reserved for genuine transport faults (§12.2).
 */
export class ToolError extends Error {
  readonly code: ErrorCode;
  readonly hint: string | undefined;
  readonly details: unknown;

  constructor(code: ErrorCode, message: string, hint?: string, details?: unknown) {
    super(message);
    this.name = 'ToolError';
    this.code = code;
    this.hint = hint;
    this.details = details;
  }
}

/** `hint` always names a remediation — a code the model can branch on plus a way out (R6). */
export function fail(code: ErrorCode, message: string, hint?: string, details?: unknown): ToolError {
  return new ToolError(code, message, hint, details);
}

export function isErrorCode(code: string): code is ErrorCode {
  return (ERROR_CODES as readonly string[]).includes(code);
}
