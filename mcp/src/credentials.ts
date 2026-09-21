/**
 * The API secret key — pm/apis.mdx §5, pm/mcp.mdx §6.
 *
 * The credentials logic exists three times — the server (pkg/machine/credentials.go), the CLI
 * (cli/internal/credentials/) and here — and cannot share source across two languages, so the
 * three share BEHAVIOUR: pkg/machine/testdata/credentials_vectors.json lists inputs and expected
 * outcomes, and test/credentials.test.ts runs every vector through this file.
 *
 * The rules, once:
 *   - 32 bytes from crypto.randomBytes rendered as 64 lowercase hex; validated with ^[0-9a-f]{64}$
 *     on every read, never padded, trimmed or lowercased.
 *   - ~/.credentials/ezbookkeeping.json, mode 0600, owned by this uid, a regular file (never a
 *     symlink, never a FIFO). Anything looser refuses with the exact chmod.
 *   - The write merges: other products' top-level keys are preserved byte for byte.
 *   - The write is atomic (temp in the same directory, wx at 0600 from creation, fsync, rename) and
 *     refuses a symlink at the destination.
 *   - The mint is a compare-and-set: re-read inside the write, yield to a key that appeared, then
 *     read back and trust the file rather than our own mint.
 *   - The fingerprint (first 4 hex + a SHA-256 prefix) is the ONLY representation of the key that
 *     may be logged, printed, put in an error or shown to a model.
 */
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

export const PRODUCT = 'ezbookkeeping';

/** 64 lowercase hex characters — 32 bytes from a CSPRNG (apis.mdx §5.2). */
const KEY_PATTERN = /^[0-9a-f]{64}$/;

export type MachineCredentials = {
  api_key?: string;
  created?: string;
  created_by?: string;
  label?: string;
  username?: string;
};

export type ProductCredentials = {
  machine?: MachineCredentials;
  statements?: { root?: string };
  timezone?: string;
};

type CredentialsFile = Record<string, unknown> & { ezbookkeeping?: ProductCredentials };

export class CredentialsError extends Error {
  readonly fix: string;

  constructor(message: string, fix: string) {
    super(message);
    this.name = 'CredentialsError';
    this.fix = fix;
  }
}

export function defaultCredentialsPath(): string {
  return path.join(os.homedir(), '.credentials', `${PRODUCT}.json`);
}

export function isWellFormedKey(key: string): boolean {
  return KEY_PATTERN.test(key);
}

/** 32 bytes from crypto.randomBytes (a CSPRNG), never Math.random. */
export function mintKey(): string {
  return crypto.randomBytes(32).toString('hex');
}

/** The Go copies' Fingerprint(): first four hex characters plus a SHA-256 prefix; "none" for no key. */
export function fingerprint(key: string): string {
  if (key.length < 4) {
    return 'none';
  }
  const digest = crypto.createHash('sha256').update(key).digest('hex');
  return `${key.slice(0, 4)}…/sha256:${digest.slice(0, 4)}`;
}

/**
 * The key-file integrity check (§7.7): a regular file, not a symlink, mode 0600, owned by the
 * current uid. Any failure refuses with the exact chmod or path in the message. `lstat`, not
 * `stat`: a symlink is somebody redirecting our secret, and following it would be exactly wrong.
 * Returns false when the file does not exist (not an error: it may be minted).
 */
export function checkCredentialsFile(file: string): boolean {
  let stat: fs.Stats;
  try {
    stat = fs.lstatSync(file);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') {
      return false;
    }
    throw new CredentialsError(`cannot stat ${file}: ${(err as Error).message}`, `check that ${path.dirname(file)} is readable`);
  }

  if (stat.isSymbolicLink()) {
    throw new CredentialsError(`credentials file ${file} is a symlink; refusing to follow it`, `rm ${file} and let the app re-mint the key`);
  }
  if (!stat.isFile()) {
    throw new CredentialsError(`credentials file ${file} is not a regular file`, `rm ${file} and let the app re-mint the key`);
  }
  if ((stat.mode & 0o077) !== 0) {
    const mode = (stat.mode & 0o777).toString(8).padStart(3, '0');
    throw new CredentialsError(`credentials file ${file} is mode ${mode}; it must not be readable by anyone else`, `chmod 600 ${file}`);
  }
  if (typeof process.getuid === 'function' && stat.uid !== process.getuid()) {
    throw new CredentialsError(`credentials file ${file} is owned by uid ${stat.uid}, not the current user`, `chown ${process.getuid()} ${file}`);
  }
  return true;
}

function readRaw(file: string): { doc: CredentialsFile; exists: boolean } {
  const exists = checkCredentialsFile(file);
  if (!exists) {
    return { doc: {}, exists: false };
  }
  const raw = fs.readFileSync(file, 'utf8');
  if (raw.trim() === '') {
    return { doc: {}, exists: true };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    throw new CredentialsError(`credentials file ${file} is not a JSON object: ${(err as Error).message}`, `fix or remove ${file}`);
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new CredentialsError(`credentials file ${file} is not a JSON object`, `fix or remove ${file}`);
  }
  return { doc: parsed as CredentialsFile, exists: true };
}

/** This app's subtree of the file; a missing file is not an error (the key may be minted). */
export function readProduct(file: string): ProductCredentials {
  const { doc } = readRaw(file);
  const sub = doc[PRODUCT];
  if (sub === undefined) {
    return {};
  }
  if (sub === null || typeof sub !== 'object' || Array.isArray(sub)) {
    throw new CredentialsError(`the "${PRODUCT}" subtree of ${file} is not an object`, `fix or remove ${file}`);
  }
  return sub as ProductCredentials;
}

/**
 * Atomic, merging, symlink-refusing write (apis.mdx §5.3). Temp file in the SAME directory, `wx`
 * so we never land on another process's temp, 0600 from creation (never chmod after), fsync before
 * rename so a crash cannot leave a truncated secret.
 */
function writeMerged(file: string, mutate: (doc: CredentialsFile) => void): void {
  const dir = path.dirname(file);
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });

  const { doc } = readRaw(file);
  mutate(doc);

  const tmp = path.join(dir, `.${path.basename(file)}.tmp-${crypto.randomBytes(6).toString('hex')}`);
  const fd = fs.openSync(tmp, 'wx', 0o600);
  try {
    fs.writeFileSync(fd, `${JSON.stringify(doc, null, 2)}\n`, 'utf8');
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }

  let destIsLink = false;
  try {
    destIsLink = fs.lstatSync(file).isSymbolicLink();
  } catch {
    destIsLink = false;
  }
  if (destIsLink) {
    fs.unlinkSync(tmp);
    throw new CredentialsError(`credentials file ${file} is a symlink; refusing to write through it`, `rm ${file} and retry`);
  }

  fs.renameSync(tmp, file);
}

export type KeySource = 'EZBKMCP_API_KEY' | 'EZBK_API_KEY' | 'EZBKMCP_API_KEY_FILE' | 'EZBK_API_KEY_FILE' | 'credentials-file' | 'minted';

export type ResolvedKey = {
  key: string;
  source: KeySource;
  /** The credentials file consulted, whether or not the key came from it. */
  file: string;
};

function assertKeyShape(key: string, where: string): string {
  if (!isWellFormedKey(key)) {
    throw new CredentialsError(`the API secret key from ${where} is not 64 lowercase hex characters`, 'rotate it with: ezbk key rotate --yes');
  }
  return key;
}

/**
 * Resolution order, first hit wins (apis.mdx §5.5): EZBKMCP_API_KEY → EZBKMCP_API_KEY_FILE → the
 * credentials file → mint (only when asked). The EZBK_ spellings the CLI uses are honoured too.
 * Returns null when there is no key anywhere and minting was not requested.
 */
export function resolveKey(opts: { env?: NodeJS.ProcessEnv; file: string; mint: boolean; mintedBy?: string }): ResolvedKey | null {
  const env = opts.env ?? process.env;
  const file = opts.file;

  for (const name of ['EZBKMCP_API_KEY', 'EZBK_API_KEY'] as const) {
    const v = env[name]?.trim();
    if (v) {
      return { key: assertKeyShape(v, name), source: name, file };
    }
  }

  for (const name of ['EZBKMCP_API_KEY_FILE', 'EZBK_API_KEY_FILE'] as const) {
    const p = env[name]?.trim();
    if (p) {
      let contents: string;
      try {
        contents = fs.readFileSync(p, 'utf8').trim();
      } catch (err) {
        throw new CredentialsError(`${name}=${p} cannot be read: ${(err as Error).message}`, `unset ${name} or point it at a readable file`);
      }
      return { key: assertKeyShape(contents, p), source: name, file };
    }
  }

  const existing = readProduct(file).machine?.api_key;
  if (existing !== undefined && existing !== '') {
    return { key: assertKeyShape(existing, file), source: 'credentials-file', file };
  }

  if (!opts.mint) {
    return null;
  }

  const minted = mintKey();
  writeMerged(file, doc => {
    // Compare-and-set: re-read inside the write and yield to a key that appeared meanwhile.
    const product = (doc[PRODUCT] ??= {}) as ProductCredentials;
    const machine = (product.machine ??= {});
    if (machine.api_key !== undefined && isWellFormedKey(machine.api_key)) {
      return;
    }
    machine.api_key = minted;
    machine.created = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
    machine.created_by = opts.mintedBy ?? 'mcp';
    if (machine.label === undefined) {
      const host = os.hostname();
      if (host !== '') {
        machine.label = host;
      }
    }
  });

  // Whoever wrote first wins; read back rather than trusting our own mint.
  const settled = readProduct(file).machine?.api_key ?? minted;
  return { key: assertKeyShape(settled, file), source: 'minted', file };
}
