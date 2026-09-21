/**
 * The instructions pipeline's runtime half — pm/mcp.mdx §8.3.
 *
 * scripts/build-instructions.ts reads ai/mcp_prompt_ezbookkeeping.md at build time, checks every
 * `{TOKEN}` against PROMPT_TOKENS (an unknown one fails the build naming it) and writes the prose
 * into src/instructions.ts. The substitution itself happens here, at startup, because two of the
 * values are only known then: the tool counts come from the registry (so the prose can never claim
 * a count the catalogue does not have) and the API URL and credentials file come from the config.
 *
 * EZBKMCP_PROMPT_FILE is the tuning seam: read that file instead of the generated constant, same
 * substitution, same failure on an unknown token; a missing or unreadable override logs one stderr
 * warning and falls back to the generated constant. It never ships an empty block.
 */
import fs from 'node:fs';

import { READ_TOOLS, TOTAL_TOOLS, WRITE_TOOL_COUNT } from './tools/registry.js';

/** The closed token map (§8.3). Adding one here and in the prompt is a deliberate change. */
export const PROMPT_TOKENS = ['SERVER_KEY', 'TOOL_PREFIX', 'TOTAL_TOOLS', 'READ_TOOLS', 'WRITE_TOOLS', 'CLI_BINARY', 'API_URL', 'CREDENTIALS_FILE', 'APP_URL'] as const;
export type PromptToken = (typeof PROMPT_TOKENS)[number];

export const SERVER_KEY = 'ezbookkeeping';
export const TOOL_PREFIX = 'ezb_';
export const CLI_BINARY = 'ezbk';

const TOKEN_PATTERN = /\{([A-Z][A-Z0-9_]*)\}/g;

export class PromptError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'PromptError';
  }
}

/** Every `{TOKEN}` in the text; an unknown one is an error naming it. */
export function findUnknownTokens(text: string): string[] {
  const unknown = new Set<string>();
  for (const match of text.matchAll(TOKEN_PATTERN)) {
    const token = match[1] ?? '';
    if (!(PROMPT_TOKENS as readonly string[]).includes(token)) {
      unknown.add(token);
    }
  }
  return [...unknown].sort();
}

export type PromptValues = {
  apiUrl: string;
  credentialsFile: string;
};

export function tokenMap(values: PromptValues): Record<PromptToken, string> {
  return {
    SERVER_KEY,
    TOOL_PREFIX,
    TOTAL_TOOLS: String(TOTAL_TOOLS),
    READ_TOOLS: String(READ_TOOLS),
    WRITE_TOOLS: String(WRITE_TOOL_COUNT),
    CLI_BINARY,
    API_URL: values.apiUrl,
    CREDENTIALS_FILE: values.credentialsFile,
    APP_URL: values.apiUrl,
  };
}

/** Substitute every token, or throw naming the unknown ones. */
export function substitute(text: string, values: PromptValues): string {
  if (text.trim() === '') {
    throw new PromptError('the instructions prompt is empty');
  }
  const unknown = findUnknownTokens(text);
  if (unknown.length > 0) {
    throw new PromptError(`unknown token(s) in the instructions prompt: ${unknown.map(t => `{${t}}`).join(', ')}; the known tokens are ${PROMPT_TOKENS.map(t => `{${t}}`).join(', ')}`);
  }
  const map = tokenMap(values);
  return text.replace(TOKEN_PATTERN, (_m, token: string) => map[token as PromptToken]).trim();
}

/** The default values the generated constant is built with. */
export const DEFAULT_PROMPT_VALUES: PromptValues = { apiUrl: 'http://127.0.0.1:8080', credentialsFile: '~/.credentials/ezbookkeeping.json' };

/**
 * Resolve the instructions text for this process: the EZBKMCP_PROMPT_FILE override when it can be
 * read and substituted, else the generated source. `warn` receives the one line for a failed override.
 */
export function resolveInstructionsFrom(source: string, values: PromptValues, promptFile: string | undefined, warn: (line: string) => void): string {
  if (promptFile !== undefined) {
    try {
      const text = fs.readFileSync(promptFile, 'utf8');
      return substitute(text, values);
    } catch (err) {
      warn(`EZBKMCP_PROMPT_FILE=${promptFile} was not used (${(err as Error).message}); falling back to the built-in instructions`);
    }
  }
  return substitute(source, values);
}

/** Every `ezb_` name the prose mentions (so a test can assert each exists in the registry). */
export function toolNamesMentioned(text: string): string[] {
  const names = new Set<string>();
  for (const m of text.matchAll(/\bezb_[a-z_]+/g)) {
    names.add(m[0]);
  }
  return [...names].sort();
}
