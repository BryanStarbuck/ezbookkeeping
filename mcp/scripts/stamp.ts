#!/usr/bin/env node
/**
 * Stamp dist/index.js: prepend the `#!/usr/bin/env node` banner and chmod +x — pm/mcp.mdx §5.3.
 * Claude Code spawns the server with a PATH unrelated to any shell profile, so the registration
 * names the file directly and no `node` goes in front.
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const entry = path.resolve(here, '..', 'dist', 'index.js');

const BANNER = '#!/usr/bin/env node\n';

const source = fs.readFileSync(entry, 'utf8');
if (!source.startsWith(BANNER)) {
  fs.writeFileSync(entry, BANNER + source, 'utf8');
}
fs.chmodSync(entry, 0o755);
process.stderr.write(`stamp: ${path.relative(process.cwd(), entry)} is executable\n`);
