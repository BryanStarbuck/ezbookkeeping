/**
 * T5 — PII / statement exfiltration (pm/mcp.mdx §7.0, §7.4, AC 15): no tool returns raw statement
 * text, a full account number, picture bytes, a key, or a path outside the configured roots; the
 * statements tools' descriptions say the archive is read-only; the recorded envelopes hold none of
 * those things.
 */
import fs from 'node:fs';
import path from 'node:path';

import { describe, expect, it } from 'vitest';

import { INSTRUCTIONS } from '../instructions.js';
import { TOOLS } from '../tools/registry.js';

import { MCP_ROOT, ownSourceFiles, read, walkJson } from './_shared.js';

const FIXTURES = path.join(MCP_ROOT, 'test', 'fixtures', 'envelopes');
const HEX_KEY = /[0-9a-f]{64}/;

function recordings(): Array<{ file: string; envelope: Record<string, unknown> }> {
  if (!fs.existsSync(FIXTURES)) {
    return [];
  }
  return fs
    .readdirSync(FIXTURES)
    .filter(f => f.endsWith('.json'))
    .map(f => ({ file: f, ...(JSON.parse(fs.readFileSync(path.join(FIXTURES, f), 'utf8')) as { envelope: Record<string, unknown> }) }));
}

describe('canary: redaction', () => {
  it('no tool offers raw statement text, picture bytes or a full account number', () => {
    for (const t of TOOLS) {
      const props = Object.keys(t.inputSchema.properties as Record<string, unknown>);
      expect(props, t.name).not.toContain('raw');
      expect(props, t.name).not.toContain('raw_text');
      expect(props, t.name).not.toContain('include_raw');
      expect(props, t.name).not.toContain('account_number');
      expect(t.name).not.toMatch(/picture|receipt|raw|upload/);
    }
  });

  it('the statements tools say the archive is read-only and rows are parsed, never the document', () => {
    const manifest = TOOLS.find(t => t.name === 'ezb_get_statement_manifest');
    const rows = TOOLS.find(t => t.name === 'ezb_get_statement_rows');
    const scan = TOOLS.find(t => t.name === 'ezb_scan_statements');
    expect(manifest?.description).toMatch(/read-only/);
    expect(scan?.description).toMatch(/read-only/);
    expect(rows?.description).toMatch(/never (the )?raw/);
  });

  it('the source, the schemas and the instructions carry no /Users/ path, no 64-hex secret and no real archive name', () => {
    for (const f of ownSourceFiles()) {
      const text = read(f);
      expect(text, f).not.toMatch(/\/Users\//);
      expect(text, f).not.toMatch(HEX_KEY);
      expect(text, f).not.toMatch(/Bryan_Arindom|bank_statements\//);
    }
    const schemas = JSON.stringify(TOOLS.map(t => [t.description, t.inputSchema]));
    expect(schemas).not.toMatch(/\/Users\//);
    expect(INSTRUCTIONS).not.toMatch(/\/Users\//);
    expect(INSTRUCTIONS).not.toMatch(HEX_KEY);
  });

  it('the recorded envelopes hold no key, no secret, no password, no full card number and no picture bytes', () => {
    for (const r of recordings()) {
      const json = JSON.stringify(r.envelope);
      expect(json, r.file).not.toMatch(HEX_KEY);
      expect(json, r.file).not.toMatch(/"(password|secret|salt|token_secret|api_key|apiKey)":/);
      expect(json, r.file).not.toMatch(/\b\d{4}[ -]\d{4}[ -]\d{4}[ -]\d{4}\b/);
      expect(json, r.file).not.toMatch(/"(pictureData|imageData|bytes|base64)":/);
      walkJson(r.envelope, (obj, _t, keyPath) => {
        for (const [k, v] of Object.entries(obj)) {
          if (typeof v === 'string' && /^(rawText|raw_text|statementText|document)$/.test(k)) {
            expect.fail(`${r.file} ${keyPath}.${k} carries raw statement text`);
          }
        }
      });
    }
  });

  it('the key never reaches a tool result, an error or the audit line (only its fingerprint)', () => {
    const server = read(path.join(MCP_ROOT, 'src', 'server.ts'));
    expect(server).not.toMatch(/\bkey:|\.key\b|\bkey\s*=/); // the host holds a fingerprint, never the key
    const audit = read(path.join(MCP_ROOT, 'src', 'audit.ts'));
    expect(audit).toContain('keyFingerprint');
    expect(audit).not.toMatch(/record\.key\b/);
  });
});
