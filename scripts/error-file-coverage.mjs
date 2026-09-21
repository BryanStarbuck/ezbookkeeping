#!/usr/bin/env node
// Error-file coverage report — pm/error_err.mdx §13.3.
//
// Walks every source file in scope (§13.4). For Go it builds bin/errfilecheck and runs it with
// `-json -errfile.report-sites ./...` from the repo root and from cli/; for TypeScript and Vue it
// runs `eslint --format json` limited to errfile/catch-must-report (scripts/eslint/coverage.config.mjs)
// and counts sites with a cheap textual parse. Each file lands in one of four classes:
//
//   compliant        ≥1 error site, every site passes the rule
//   net-covered      0 local error sites; the runtime's global net (§8) covers it
//   violating        ≥1 site fails the rule
//   unwired-runtime  the file's runtime has no net installed — a hard failure (§8, §13.3)
//
// (a fifth, `not-measured`, appears only for Go files when bin/errfilecheck could not be built —
// it never fails the run, but it is never "compliant" either.)
//
// It prints totals, writes ~/T/ezbookkeeping/error_file_coverage.json (outside the repo, never
// inside it), and exits non-zero when any file is violating or unwired. `just check-errors` runs it.
//
//   node scripts/error-file-coverage.mjs                 report + exit code
//   node scripts/error-file-coverage.mjs --quiet         totals only
//   node scripts/error-file-coverage.mjs --canary        also provoke one fault per runtime (§15.3)
//   node scripts/error-file-coverage.mjs --partition 12  also write ~/T/ezbookkeeping/partition/part_NN.txt (§16.3)

import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const args = process.argv.slice(2);
const quiet = args.includes('--quiet');
const canary = args.includes('--canary');
const partitionIndex = args.indexOf('--partition');
const partitionCount = partitionIndex >= 0 ? Number.parseInt(args[partitionIndex + 1] ?? '12', 10) : 0;

const RULE = 'errfile/catch-must-report';
const OUT_DIR = join(homedir(), 'T', 'ezbookkeeping');
const OUT_FILE = join(OUT_DIR, 'error_file_coverage.json');
const PARTITION_DIR = join(OUT_DIR, 'partition');

// ---------------------------------------------------------------------------------------------
// Scope (§13.4)
// ---------------------------------------------------------------------------------------------

const GO_ROOTS = ['pkg', 'cmd', 'cli'];
const GO_FILES = ['ezbookkeeping.go'];
const TS_ROOTS = ['src', 'mcp/src'];

const EXCLUDED_PREFIXES = [
    'pkg/errfile/',
    'cli/internal/errfile/',
    'src/lib/errfile/',
    'mcp/src/errfile/',
    'mcp/test/',
    'mcp/src/canary/',
    'scripts/',
    'docker/'
];

const EXCLUDED_DIR_NAMES = new Set(['testdata', '__tests__', 'node_modules', 'dist', 'fixtures']);

const GENERATED = /^\/\/ (?:GENERATED|Code generated .* DO NOT EDIT\.)/m;

function isExcludedFile(rel) {
    if (EXCLUDED_PREFIXES.some(prefix => rel.startsWith(prefix))) {
        return true;
    }

    if (rel === 'mcp/src/instructions.ts' || rel.startsWith('src/global.d.ts') || rel.startsWith('src/vue-shim.d.ts')) {
        return true;
    }

    const base = rel.slice(rel.lastIndexOf('/') + 1);

    if (base.endsWith('_test.go') || /\.(test|spec|canary)\.[^.]+$/.test(base) || base.endsWith('.d.ts')) {
        return true;
    }

    if (/\.config\.[cm]?[jt]s$/.test(base) || /^build\./.test(base)) {
        return true;
    }

    return false;
}

function walk(dir, extensions, out) {
    if (!existsSync(dir)) {
        return;
    }

    for (const entry of readdirSync(dir, { withFileTypes: true })) {
        const abs = join(dir, entry.name);

        if (entry.isDirectory()) {
            if (!EXCLUDED_DIR_NAMES.has(entry.name)) {
                walk(abs, extensions, out);
            }
        } else if (entry.isFile()) {
            const dot = entry.name.lastIndexOf('.');

            if (dot !== -1 && extensions.has(entry.name.slice(dot))) {
                out.push(abs);
            }
        }
    }
}

function collectInScope() {
    const abs = [];

    for (const r of GO_ROOTS) {
        walk(join(root, r), new Set(['.go']), abs);
    }

    for (const f of GO_FILES) {
        if (existsSync(join(root, f))) {
            abs.push(join(root, f));
        }
    }

    for (const r of TS_ROOTS) {
        walk(join(root, r), new Set(['.ts', '.vue']), abs);
    }

    const rels = new Set();

    for (const p of abs) {
        const rel = relative(root, p).replace(/\\/g, '/');

        if (isExcludedFile(rel)) {
            continue;
        }

        let head = '';

        try {
            head = readFileSync(p, 'utf8').slice(0, 400);
        } catch {
            head = '';
        }

        if (GENERATED.test(head)) {
            continue;
        }

        rels.add(rel);
    }

    return [...rels].sort();
}

// ---------------------------------------------------------------------------------------------
// Runtimes and their nets (§8, §13.3)
// ---------------------------------------------------------------------------------------------

function fileHas(rel, marker) {
    const p = join(root, rel);
    return existsSync(p) && readFileSync(p, 'utf8').includes(marker);
}

/** Each runtime names the markers that prove its net is wired. */
const RUNTIMES = {
    web: {
        nets: ['N1', 'N2', 'N4', 'N15'],
        markers: [
            ['src/desktop-main.ts', 'installBrowserErrorFile('],
            ['src/mobile-main.ts', 'installBrowserErrorFile(']
        ]
    },
    sw: {
        nets: ['N3'],
        markers: [['src/sw.ts', 'installBrowserErrorFile(']]
    },
    server: {
        nets: ['N5', 'N6', 'N7', 'N8', 'N9', 'N10', 'N11', 'N12'],
        markers: [
            ['cmd/initializer.go', 'errfile.Install('],
            ['cmd/initializer.go', 'InstallLogHook('],
            ['pkg/log/logger.go', 'func AddHook(']
        ]
    },
    'app-cli': {
        nets: ['N5', 'N6', 'N11'],
        markers: [
            ['cmd/initializer.go', 'errfile.Install('],
            ['cmd/initializer.go', 'InstallLogHook('],
            ['pkg/log/logger.go', 'func AddHook(']
        ]
    },
    ezbk: {
        nets: ['N13'],
        markers: [['cli/cmd/ezbk/main.go', 'errfile.Install(']]
    },
    mcp: {
        nets: ['N14'],
        markers: [['mcp/src/index.ts', 'installNodeErrorFile(']]
    }
};

/** Which runtime a file runs in, by its location. app-cli shares the server binary's files. */
function runtimeOf(rel) {
    if (rel === 'src/sw.ts') {
        return 'sw';
    }

    if (rel.startsWith('src/')) {
        return 'web';
    }

    if (rel.startsWith('mcp/')) {
        return 'mcp';
    }

    if (rel.startsWith('cli/')) {
        return 'ezbk';
    }

    return 'server';
}

function wiredRuntimes() {
    const result = {};

    for (const [name, { markers }] of Object.entries(RUNTIMES)) {
        const missing = markers.filter(([rel, marker]) => !fileHas(rel, marker)).map(([rel, marker]) => `${rel}: ${marker}`);
        result[name] = { wired: missing.length === 0, missing };
    }

    return result;
}

// ---------------------------------------------------------------------------------------------
// Go — bin/errfilecheck (§13.1)
// ---------------------------------------------------------------------------------------------

function buildErrfilecheck() {
    const moduleDir = join(root, 'scripts', 'errfilecheck');
    const bin = join(root, 'bin', 'errfilecheck');

    if (!existsSync(join(moduleDir, 'main.go'))) {
        return { bin: null, reason: 'scripts/errfilecheck/ is absent (B1 builds it) — Go not measured' };
    }

    try {
        mkdirSync(join(root, 'bin'), { recursive: true });
        execFileSync('go', ['build', '-o', bin, '.'], { cwd: moduleDir, stdio: ['ignore', 'pipe', 'pipe'], encoding: 'utf8' });
        return { bin, reason: '' };
    } catch (e) {
        return { bin: null, reason: `go build of scripts/errfilecheck failed: ${String(e.stderr || e.message).trim().split('\n')[0]} — Go not measured` };
    }
}

/** Run the analyzer over one module; returns Map<rel, { sites, violations, diagnostics }>. */
function goDiagnostics(bin, cwd) {
    const result = spawnSync(bin, ['-json', '-errfile.report-sites', './...'], {
        cwd,
        encoding: 'utf8',
        maxBuffer: 256 * 1024 * 1024
    });
    const stdout = result.stdout || '';
    const perFile = new Map();

    if (!stdout.trim().startsWith('{')) {
        throw new Error(`errfilecheck produced no JSON in ${relative(root, cwd) || '.'}: ${(result.stderr || '').trim().split('\n')[0]}`);
    }

    const parsed = JSON.parse(stdout);

    for (const analyzers of Object.values(parsed)) {
        for (const diagnostics of Object.values(analyzers)) {
            if (!Array.isArray(diagnostics)) {
                continue;
            }

            for (const d of diagnostics) {
                const posn = String(d.posn || '');
                const m = /^(.*?):(\d+):(\d+)$/.exec(posn);

                if (!m) {
                    continue;
                }

                const rel = relative(root, m[1]).replace(/\\/g, '/');
                const entry = perFile.get(rel) || { sites: 0, violations: 0, diagnostics: [] };

                if (d.category === 'site') {
                    entry.sites += 1;
                } else {
                    entry.violations += 1;
                    entry.diagnostics.push({ line: Number(m[2]), column: Number(m[3]), category: d.category || '', message: d.message });
                }

                perFile.set(rel, entry);
            }
        }
    }

    return perFile;
}

function analyseGo() {
    const { bin, reason } = buildErrfilecheck();

    if (!bin) {
        return { measured: false, reason, perFile: new Map() };
    }

    const perFile = new Map();

    for (const cwd of [root, join(root, 'cli')]) {
        if (!existsSync(join(cwd, 'go.mod'))) {
            continue;
        }

        try {
            for (const [rel, entry] of goDiagnostics(bin, cwd)) {
                perFile.set(rel, entry);
            }
        } catch (e) {
            return { measured: false, reason: `${e.message} — Go not measured`, perFile: new Map() };
        }
    }

    return { measured: true, reason: '', perFile };
}

// ---------------------------------------------------------------------------------------------
// TypeScript and Vue — the lint rule (§13.2) and the cheap site count
// ---------------------------------------------------------------------------------------------

const SITE_PATTERNS = [
    /(?<![.\w$])catch\s*[({]/g,
    /\.catch\s*\(/g,
    /addEventListener\(\s*['"](?:error|unhandledrejection)['"]/g,
    /\.on(?:error|abort)\s*=[^=]/g
];

function stripComments(src) {
    return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:'"`\\])\/\/.*$/gm, '$1');
}

function countTsSites(rel) {
    const src = stripComments(readFileSync(join(root, rel), 'utf8'));
    let n = 0;

    for (const re of SITE_PATTERNS) {
        re.lastIndex = 0;
        n += (src.match(re) || []).length;
    }

    return n;
}

/** Run eslint (the coverage config, this rule only) over `targets`, from `cwd`; Map<rel, diagnostics[]>. */
function eslintViolations(cwd, targets, configPath) {
    const perFile = new Map();

    if (!targets.length) {
        return perFile;
    }

    const result = spawnSync('npx', ['eslint', '--no-inline-config', '--config', configPath, '--format', 'json', ...targets], {
        cwd,
        encoding: 'utf8',
        maxBuffer: 256 * 1024 * 1024,
        env: { ...process.env, NODE_OPTIONS: `${process.env.NODE_OPTIONS || ''} --max-old-space-size=4096`.trim() }
    });
    const stdout = result.stdout || '';

    if (!stdout.trim().startsWith('[')) {
        throw new Error(`eslint produced no JSON in ${relative(root, cwd) || '.'}: ${(result.stderr || '').trim().split('\n').slice(0, 3).join(' / ')}`);
    }

    for (const file of JSON.parse(stdout)) {
        const rel = relative(root, file.filePath).replace(/\\/g, '/');
        const list = [];

        for (const m of file.messages || []) {
            if (m.ruleId !== RULE) {
                continue;
            }

            list.push({ line: m.line ?? 0, column: m.column ?? 0, category: m.messageId || '', message: m.message });
        }

        if (list.length) {
            perFile.set(rel, list);
        }
    }

    return perFile;
}

function analyseTs() {
    const configPath = join(root, 'scripts', 'eslint', 'coverage.config.mjs');
    const perFile = new Map();
    const notes = [];

    for (const [rel, diagnostics] of eslintViolations(root, ['src'], configPath)) {
        perFile.set(rel, diagnostics);
    }

    if (existsSync(join(root, 'mcp', 'package.json'))) {
        try {
            for (const [rel, diagnostics] of eslintViolations(join(root, 'mcp'), ['src'], configPath)) {
                perFile.set(rel, diagnostics);
            }
        } catch (e) {
            notes.push(`mcp/ lint from mcp/ failed (${e.message}); linted mcp/src from the root instead`);

            for (const [rel, diagnostics] of eslintViolations(root, ['mcp/src'], configPath)) {
                perFile.set(rel, diagnostics);
            }
        }
    } else {
        notes.push('mcp/package.json is absent — mcp/src skipped');
    }

    return { perFile, notes };
}

// ---------------------------------------------------------------------------------------------
// Canaries (§15.3)
// ---------------------------------------------------------------------------------------------

function runCanaries() {
    const results = {};
    const tmp = mkdtempSync(join(tmpdir(), 'ezbk-canary-'));

    try {
        // web + sw: the vitest canaries of browser.ts against a stub fetch.
        const vitest = spawnSync('npx', ['vitest', 'run', '--reporter=verbose', 'src/lib/errfile/__tests__/canary.test.ts'], {
            cwd: root,
            encoding: 'utf8',
            env: { ...process.env, VITE_CONFIG_NATIVE_IGNORE_WARNING: 'true' }
        });
        const out = `${vitest.stdout}\n${vitest.stderr}`;
        const tail = out.trim().split('\n').filter(l => /Tests|FAIL|Error/.test(l)).slice(-3).join(' / ');
        results.web = vitest.status === 0 && /✓.*canary web/.test(out) ? 'OK' : `FAILED: ${tail}`;
        results.sw = vitest.status === 0 && /✓.*canary sw/.test(out) ? 'OK' : `FAILED: ${tail}`;

        // mcp: a child process installs the node sink (from mcp/dist when built, else the TypeScript
        // source through a .js→.ts resolve hook) and reports one fault under an EZBK_ERROR_FILE override.
        const dist = join(root, 'mcp', 'dist', 'errfile', 'node.js');
        const src = join(root, 'mcp', 'src', 'errfile', 'node.ts');

        if (existsSync(dist) || existsSync(src)) {
            const file = join(tmp, 'error.err');
            const entry = existsSync(dist) ? dist : src;
            const core = existsSync(dist) ? join(root, 'mcp', 'dist', 'errfile', 'core.js') : join(root, 'mcp', 'src', 'errfile', 'core.ts');
            writeFileSync(join(tmp, 'hooks.mjs'), `
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
export async function resolve(specifier, context, next) {
    if (specifier.startsWith('.') && specifier.endsWith('.js') && context.parentURL) {
        const ts = new URL(specifier.slice(0, -3) + '.ts', context.parentURL);
        if (existsSync(fileURLToPath(ts))) return next(ts.href, context);
    }
    return next(specifier, context);
}
`);
            writeFileSync(join(tmp, 'register.mjs'), `import { register } from 'node:module';\nregister('./hooks.mjs', import.meta.url);\n`);
            writeFileSync(join(tmp, 'child.mjs'), `
const { installNodeErrorFile } = await import(${JSON.stringify(entry)});
const { errorFileFor } = await import(${JSON.stringify(core)});
installNodeErrorFile({ app: 'mcp', echo: false });
errorFileFor('mcp/src/server.ts').caught('running the canary tool', new Error('canary mcp'));
`);
            const child = spawnSync(process.execPath, ['--no-warnings', '--import', join(tmp, 'register.mjs'), join(tmp, 'child.mjs')], {
                cwd: root,
                encoding: 'utf8',
                env: { ...process.env, EZBK_ERROR_FILE: file, VITEST: '', NODE_ENV: 'production' },
                timeout: 20_000
            });
            const text = existsSync(file) ? readFileSync(file, 'utf8') : '';
            results.mcp = child.status === 0 && /\[ERROR\] \[mcp\] \[mcp\/src\/server\.ts\] running the canary tool — Error: canary mcp/.test(text)
                ? 'OK'
                : `FAILED: exit ${child.status}; ${(child.stderr || '').trim().split('\n').slice(-2).join(' / ')}`;
        } else {
            results.mcp = 'SKIPPED: mcp/src/errfile/node.ts is absent';
        }

        // app-cli: `ezbookkeeping utility __canary` under EZBK_ERROR_FILE_CANARY=1 (B1's hook).
        const appCliHook = spawnSync('grep', ['-rl', '__canary', 'cmd'], { cwd: root, encoding: 'utf8' }).stdout.trim();

        if (appCliHook) {
            const file = join(tmp, 'app-cli.err');
            const run = spawnSync('go', ['run', '.', 'utility', '__canary'], {
                cwd: root,
                encoding: 'utf8',
                env: { ...process.env, EZBK_ERROR_FILE: file, EZBK_ERROR_FILE_CANARY: '1' },
                timeout: 120_000
            });
            const text = existsSync(file) ? readFileSync(file, 'utf8') : '';
            results['app-cli'] = /\[app-cli\]/.test(text) ? 'OK' : `FAILED: exit ${run.status}; no [app-cli] line in ${file}`;
        } else {
            results['app-cli'] = 'SKIPPED: no __canary subcommand under cmd/ yet';
        }

        // ezbk: `ezbk __canary`, hidden (B1's hook).
        const ezbkHook = spawnSync('grep', ['-rl', '__canary', 'cli'], { cwd: root, encoding: 'utf8' }).stdout.trim();

        if (ezbkHook) {
            const file = join(tmp, 'ezbk.err');
            const run = spawnSync('go', ['run', './cmd/ezbk', '__canary'], {
                cwd: join(root, 'cli'),
                encoding: 'utf8',
                env: { ...process.env, EZBK_ERROR_FILE: file, EZBK_ERROR_FILE_CANARY: '1' },
                timeout: 120_000
            });
            const text = existsSync(file) ? readFileSync(file, 'utf8') : '';
            results.ezbk = /\[ezbk\]/.test(text) ? 'OK' : `FAILED: exit ${run.status}; no [ezbk] line in ${file}`;
        } else {
            results.ezbk = 'SKIPPED: no __canary verb under cli/ yet';
        }

        // server: POST /machine/v1/__canary needs a running server under EZBK_ERROR_FILE_CANARY=1;
        // pkg/machine's own test drives it (pkg/machine/errfile_test.go). This script only reports it.
        const serverHook = spawnSync('grep', ['-rl', '__canary', 'pkg/machine'], { cwd: root, encoding: 'utf8' }).stdout.trim();
        results.server = serverHook
            ? 'SKIPPED: the route exists (pkg/machine); it is driven by `go test ./pkg/machine/...`, not by this script'
            : 'SKIPPED: no __canary route under pkg/machine yet';
    } finally {
        rmSync(tmp, { recursive: true, force: true });
    }

    return results;
}

// ---------------------------------------------------------------------------------------------
// Partition (§16.3)
// ---------------------------------------------------------------------------------------------

/**
 * Split path-ordered directory groups into at most `n` contiguous segments minimising the heaviest
 * segment. A group heavier than the capacity is split at file boundaries, so a directory stays
 * with one agent unless it alone outweighs a segment.
 */
function partition(files, n) {
    const groups = [];

    for (const f of files) {
        const dir = f.file.slice(0, f.file.lastIndexOf('/'));
        const last = groups[groups.length - 1];

        if (last && last.dir === dir) {
            last.files.push(f);
            last.weight += f.weight;
        } else {
            groups.push({ dir, files: [f], weight: f.weight });
        }
    }

    function cut(capacity) {
        const segments = [];
        let current = [];
        let currentWeight = 0;

        const push = f => {
            if (currentWeight + f.weight > capacity && current.length) {
                segments.push(current);
                current = [];
                currentWeight = 0;
            }

            current.push(f);
            currentWeight += f.weight;
        };

        for (const g of groups) {
            if (g.weight <= capacity) {
                if (currentWeight + g.weight > capacity && current.length) {
                    segments.push(current);
                    current = [];
                    currentWeight = 0;
                }

                current.push(...g.files);
                currentWeight += g.weight;
            } else {
                for (const f of g.files) {
                    push(f);
                }
            }
        }

        if (current.length) {
            segments.push(current);
        }

        return segments;
    }

    const total = files.reduce((s, f) => s + f.weight, 0);
    const heaviest = files.reduce((m, f) => Math.max(m, f.weight), 0);
    let lo = heaviest;
    let hi = Math.max(total, heaviest);

    while (lo < hi) {
        const mid = Math.floor((lo + hi) / 2);

        if (cut(mid).length <= n) {
            hi = mid;
        } else {
            lo = mid + 1;
        }
    }

    const segments = cut(lo);

    // The minimal cap can leave agents idle when one directory (or file) alone sets it. Give every
    // idle agent work by splitting the heaviest segment that spans more than one directory at the
    // group boundary nearest its middle — the heaviest segment never grows, so the cap still holds.
    const dirOf = f => f.file.slice(0, f.file.lastIndexOf('/'));
    const weightOf = segment => segment.reduce((s, f) => s + f.weight, 0);

    while (segments.length < n) {
        let best = -1;
        let bestWeight = -1;

        segments.forEach((segment, i) => {
            const dirs = new Set(segment.map(dirOf));

            if (dirs.size > 1 && weightOf(segment) > bestWeight) {
                best = i;
                bestWeight = weightOf(segment);
            }
        });

        if (best < 0) {
            break;
        }

        const segment = segments[best];
        const half = weightOf(segment) / 2;
        let acc = 0;
        let cutAt = 0;
        let bestGap = Number.POSITIVE_INFINITY;

        for (let i = 1; i < segment.length; i++) {
            acc += segment[i - 1].weight;

            if (dirOf(segment[i - 1]) !== dirOf(segment[i]) && Math.abs(acc - half) < bestGap) {
                bestGap = Math.abs(acc - half);
                cutAt = i;
            }
        }

        if (cutAt === 0) {
            break;
        }

        segments.splice(best, 1, segment.slice(0, cutAt), segment.slice(cutAt));
    }

    while (segments.length < n) {
        segments.push([]);
    }

    return segments;
}

function writePartition(report, n) {
    const files = report
        .filter(r => r.violations > 0)
        .map(r => ({ file: r.file, weight: 1 + 3 * (r.sites ?? 0) }));
    const segments = partition(files, n);
    mkdirSync(PARTITION_DIR, { recursive: true, mode: 0o700 });

    for (const name of readdirSync(PARTITION_DIR)) {
        if (/^part_\d+\.txt$/.test(name)) {
            rmSync(join(PARTITION_DIR, name));
        }
    }

    segments.forEach((segment, i) => {
        const weight = segment.reduce((s, f) => s + f.weight, 0);
        const body = [`# files=${segment.length} weight=${weight}`, ...segment.map(f => f.file)].join('\n') + '\n';
        writeFileSync(join(PARTITION_DIR, `part_${String(i + 1).padStart(2, '0')}.txt`), body, { mode: 0o600 });
    });

    return segments.map((s, i) => ({ part: i + 1, files: s.length, weight: s.reduce((w, f) => w + f.weight, 0) }));
}

// ---------------------------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------------------------

const files = collectInScope();
const wired = wiredRuntimes();
const go = analyseGo();
const ts = analyseTs();

const report = [];

for (const rel of files) {
    const runtime = runtimeOf(rel);
    const isGo = rel.endsWith('.go');
    let sites;
    let found;

    if (isGo) {
        const entry = go.perFile.get(rel);
        sites = go.measured ? (entry?.sites ?? 0) : null;
        found = entry?.diagnostics ?? [];
    } else {
        found = ts.perFile.get(rel) ?? [];
        sites = Math.max(countTsSites(rel), found.length);
    }

    let cls;

    if (!wired[runtime].wired) {
        cls = 'unwired-runtime';
    } else if (isGo && !go.measured) {
        cls = 'not-measured';
    } else if (found.length > 0) {
        cls = 'violating';
    } else if ((sites ?? 0) > 0) {
        cls = 'compliant';
    } else {
        cls = 'net-covered';
    }

    report.push({ file: rel, runtime, sites, violations: found.length, class: cls, diagnostics: found });
}

const totals = { compliant: 0, 'net-covered': 0, violating: 0, 'unwired-runtime': 0, 'not-measured': 0 };

for (const r of report) {
    totals[r.class]++;
}

const unwiredRuntimes = Object.entries(wired).filter(([, w]) => !w.wired).map(([name]) => name);
const violatingAnywhere = report.filter(r => r.violations > 0).length;
const sitesTotal = report.reduce((n, r) => n + (r.sites ?? 0), 0);
const violationsTotal = report.reduce((n, r) => n + r.violations, 0);

let canaries = null;

if (canary) {
    canaries = runCanaries();
}

let parts = null;

if (partitionCount > 0) {
    parts = writePartition(report, partitionCount);
}

mkdirSync(OUT_DIR, { recursive: true, mode: 0o700 });
writeFileSync(OUT_FILE, JSON.stringify({
    generatedAt: new Date().toISOString(),
    totals: {
        files: report.length,
        ...totals,
        errorSites: sitesTotal,
        violations: violationsTotal,
        filesWithViolations: violatingAnywhere
    },
    go: { measured: go.measured, reason: go.reason },
    notes: ts.notes,
    runtimes: Object.fromEntries(Object.entries(RUNTIMES).map(([name, r]) => [name, { nets: r.nets, wired: wired[name].wired, missing: wired[name].missing }])),
    unwiredRuntimes,
    canaries,
    partition: parts,
    files: report
}, null, 2) + '\n', { mode: 0o600 });

if (!quiet) {
    const byRuntime = {};

    for (const r of report) {
        const b = (byRuntime[r.runtime] ||= { files: 0, sites: 0, violations: 0, violatingFiles: 0 });
        b.files++;
        b.sites += r.sites ?? 0;
        b.violations += r.violations;

        if (r.violations > 0) {
            b.violatingFiles++;
        }
    }

    console.log('runtime      wired  files  sites  violations  violating-files');

    for (const [name, b] of Object.entries(byRuntime)) {
        console.log(`${name.padEnd(12)} ${String(wired[name].wired ? 'yes' : 'NO').padEnd(6)} ${String(b.files).padStart(5)}  ${String(b.sites).padStart(5)}  ${String(b.violations).padStart(10)}  ${String(b.violatingFiles).padStart(15)}`);
    }

    for (const [name, w] of Object.entries(wired)) {
        if (!w.wired) {
            console.log(`  ${name}: missing ${w.missing.join('; ')}`);
        }
    }

    console.log('');

    if (!go.measured) {
        console.log(`Go: ${go.reason}`);
    }

    for (const note of ts.notes) {
        console.log(`note: ${note}`);
    }

    if (canaries) {
        console.log('canaries (§15.3):');

        for (const [name, result] of Object.entries(canaries)) {
            console.log(`  ${name.padEnd(9)} ${result}`);
        }
    }

    if (parts) {
        console.log(`partition (§16.3): ${parts.length} parts in ${PARTITION_DIR}`);

        for (const p of parts) {
            console.log(`  part_${String(p.part).padStart(2, '0')}  files=${p.files}  weight=${p.weight}`);
        }
    }
}

console.log(`error-file coverage (pm/error_err.mdx §13.3) — ${report.length} files in scope`);
console.log(`  compliant        ${totals.compliant}`);
console.log(`  net-covered      ${totals['net-covered']}`);
console.log(`  violating        ${totals.violating}`);
console.log(`  unwired-runtime  ${totals['unwired-runtime']}${unwiredRuntimes.length ? `  (${unwiredRuntimes.join(', ')})` : ''}`);

if (totals['not-measured'] > 0) {
    console.log(`  not-measured     ${totals['not-measured']}  (Go: ${go.reason})`);
}

console.log(`  error sites ${sitesTotal}, violations ${violationsTotal} in ${violatingAnywhere} files`);
console.log(`  report: ${OUT_FILE}`);

if (totals.violating > 0 || totals['unwired-runtime'] > 0 || (canaries && Object.values(canaries).some(v => v.startsWith('FAILED')))) {
    process.exitCode = 1;
}
