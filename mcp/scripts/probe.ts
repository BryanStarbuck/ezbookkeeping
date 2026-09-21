#!/usr/bin/env node
/**
 * The hand-piped probe — pm/mcp.mdx §17: initialize, then tools/list, against the built server over
 * stdio. Prints the initialize result and the tool count; every stdout line the server produced must
 * have parsed as JSON-RPC or the probe fails. (This is a script, not the server: it may print.)
 */
import { spawn } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const entry = path.resolve(here, '..', 'dist', 'index.js');

const child = spawn(process.execPath, [entry, 'serve'], { stdio: ['pipe', 'pipe', 'inherit'] });
let buffer = '';
const answers: Array<Record<string, unknown>> = [];

child.stdout.setEncoding('utf8');
child.stdout.on('data', (chunk: string) => {
  buffer += chunk;
  let idx = buffer.indexOf('\n');
  while (idx >= 0) {
    const line = buffer.slice(0, idx);
    buffer = buffer.slice(idx + 1);
    if (line.trim() !== '') {
      try {
        answers.push(JSON.parse(line) as Record<string, unknown>);
      } catch {
        console.error(`probe: stdout carried a non-JSON line: ${line.slice(0, 200)}`);
        process.exitCode = 1;
      }
    }
    idx = buffer.indexOf('\n');
  }
});

const send = (msg: Record<string, unknown>): void => {
  child.stdin.write(`${JSON.stringify(msg)}\n`);
};

send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'probe', version: '1' } } });
send({ jsonrpc: '2.0', method: 'notifications/initialized', params: {} });
send({ jsonrpc: '2.0', id: 2, method: 'tools/list', params: {} });

setTimeout(() => {
  child.stdin.end();
  child.kill('SIGTERM');
}, 1500);

child.on('exit', () => {
  const init = answers.find(a => a.id === 1);
  const list = answers.find(a => a.id === 2);
  if (init === undefined || list === undefined) {
    console.error('probe: no answer to initialize or tools/list; see the banner above');
    process.exit(1);
  }
  const result = init.result as { serverInfo: unknown; capabilities: unknown; instructions: string };
  console.log(JSON.stringify({ serverInfo: result.serverInfo, capabilities: result.capabilities, instructionsFirstSentence: result.instructions.split('. ')[0] }, null, 2));
  const tools = (list.result as { tools: Array<{ name: string }> }).tools;
  console.log(`tools/list: ${String(tools.length)} tools (${tools.filter(t => t.name.startsWith('ezb_')).length.toString()} ezb_)`);
});
