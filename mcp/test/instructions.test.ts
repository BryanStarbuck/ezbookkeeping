/**
 * Instructions freshness — pm/mcp.mdx §8.3, §17 "Instructions freshness", AC 4, 7b.
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { KNOWN_TOKENS, OUTPUT_PATH, PROMPT_PATH, generate } from '../scripts/build-instructions.js';
import { INSTRUCTIONS, PROMPT_SOURCE, resolveInstructions } from '../src/instructions.js';
import { DEFAULT_PROMPT_VALUES, PROMPT_TOKENS, PromptError, findUnknownTokens, substitute, toolNamesMentioned } from '../src/prompt.js';
import { findTool, READ_TOOLS, TOTAL_TOOLS, WRITE_TOOL_COUNT } from '../src/tools/registry.js';

import { tempDir } from './helpers/fake.js';

const here = path.dirname(fileURLToPath(import.meta.url));

const LAYER_5 =
  "Is this about the operator's own money kept in **ezBookkeeping** on THIS computer — its accounts, transactions, categories, tags, scheduled transactions, exchange rates and bank-statement imports? That is this server. The operator's envelope budget in **Actual Budget** — budget months, To Budget, covering overspending — is the `actual_budget` server. A company's bookkeeping — invoices, bills, vendors, customers, journal entries, P&L — is the `quickbooks` server. A film project — scenes, shots, takes, render credits — is the `act3` server.";

describe('the instructions pipeline', () => {
  it('has a generated file that matches what the prompt generates now', () => {
    const prompt = fs.readFileSync(PROMPT_PATH, 'utf8');
    const expected = generate(prompt, PROMPT_PATH);
    const actual = fs.readFileSync(OUTPUT_PATH, 'utf8');
    expect(actual, 'src/instructions.ts is stale: run `npm run build`').toBe(expected);
    expect(PROMPT_SOURCE).toBe(prompt);
  });

  it('keeps the generator\'s token list and the runtime token map identical', () => {
    expect([...KNOWN_TOKENS].sort()).toEqual([...PROMPT_TOKENS].sort());
  });

  it('fails the build on an empty prompt or an unknown token, naming it', () => {
    expect(() => generate('', PROMPT_PATH)).toThrow(/empty/);
    expect(() => generate('   \n', PROMPT_PATH)).toThrow(/empty/);
    const bad = `${LAYER_5}\n\nHello {NOT_A_TOKEN} and {ANOTHER}.`;
    expect(() => generate(bad, PROMPT_PATH)).toThrow(/\{ANOTHER\}, \{NOT_A_TOKEN\}/);
    expect(() => substitute(bad, DEFAULT_PROMPT_VALUES)).toThrow(PromptError);
    expect(findUnknownTokens(bad)).toEqual(['ANOTHER', 'NOT_A_TOKEN']);
    expect(() => generate('Not the routing sentence.', PROMPT_PATH)).toThrow(/routing sentence/);
  });

  it('starts with §3.4 layer 5 verbatim, naming Actual Budget (AC 4)', () => {
    expect(INSTRUCTIONS.startsWith(LAYER_5)).toBe(true);
    expect(INSTRUCTIONS).toContain('Actual Budget');
  });

  it('substitutes every token, with counts from the registry', () => {
    expect(INSTRUCTIONS).not.toMatch(/\{[A-Z_]+\}/);
    expect(INSTRUCTIONS).toContain(`There are ${String(TOTAL_TOOLS)} of them: ${String(READ_TOOLS)} read and ${String(WRITE_TOOL_COUNT)} write.`);
    expect(INSTRUCTIONS).toContain('`ezb_something`');
    expect(INSTRUCTIONS).toContain('http://127.0.0.1:8080');
    expect(INSTRUCTIONS).toContain('ezbk up');
  });

  it('mentions only tools that exist (AC 7b)', () => {
    const names = toolNamesMentioned(INSTRUCTIONS);
    expect(names.length).toBeGreaterThan(10);
    for (const name of names) {
      expect(findTool(name), `${name} is mentioned in ai/mcp_prompt_ezbookkeeping.md but is not in the registry`).toBeDefined();
    }
  });

  it('holds no fact about the books: no /Users/ path and no hex secret', () => {
    expect(PROMPT_SOURCE).not.toContain('/Users/');
    expect(PROMPT_SOURCE).not.toMatch(/[0-9a-f]{64}/);
  });

  it('honours EZBKMCP_PROMPT_FILE, falling back with one warning when it cannot be used', () => {
    const dir = tempDir('ezbkmcp-prompt-');
    const override = path.join(dir, 'p.md');
    fs.writeFileSync(override, `${LAYER_5}\n\nOverride for {SERVER_KEY} with {TOTAL_TOOLS} tools.`);
    const warnings: string[] = [];
    const text = resolveInstructions({ apiUrl: 'http://127.0.0.1:9', credentialsFile: '/x' }, override, w => warnings.push(w));
    expect(text).toContain(`Override for ezbookkeeping with ${String(TOTAL_TOOLS)} tools.`);
    expect(warnings).toEqual([]);

    const missing = resolveInstructions(DEFAULT_PROMPT_VALUES, path.join(dir, 'missing.md'), w => warnings.push(w));
    expect(missing).toBe(INSTRUCTIONS);
    expect(warnings.length).toBe(1);
    expect(warnings[0]).toContain('falling back');

    fs.writeFileSync(override, 'bad {NOPE}');
    const bad = resolveInstructions(DEFAULT_PROMPT_VALUES, override, w => warnings.push(w));
    expect(bad).toBe(INSTRUCTIONS);
    expect(warnings[1]).toContain('{NOPE}');
  });

  it('is served in exactly one place: the initialize result (no resource, no prompt)', () => {
    const serverSource = fs.readFileSync(path.resolve(here, '..', 'src', 'server.ts'), 'utf8');
    expect(serverSource).toContain('instructions: opts.instructions');
    expect(serverSource).not.toMatch(/ListResourcesRequestSchema|ListPromptsRequestSchema/);
  });
});
