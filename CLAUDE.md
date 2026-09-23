# ezBookkeeping (Bryan's fork)

~/BGit/Bryan_git/ezbookkeeping/
This directory is our web app. It is open source (https://github.com/BryanStarbuck/ezbookkeeping.git), so we never put private data in this directory or anywhere under it.

Run it with the `justfile` in this directory: `just build` then `just run` (serves http://localhost:8080/).

* Upstream: https://github.com/mayswind/ezbookkeeping (MIT). Our `origin` remote is the fork.
* Stack: Go server (gin + xorm, module `github.com/mayswind/ezbookkeeping`) and a Vue 3 + Vite front end, with a desktop edition (Vuetify) and a mobile edition (Framework7). Uses SQLite by default. The upstream version at fork time was 2.0.1.
* Sister fork: ~/BGit/Bryan_git/actual_budget_bryan/ (Actual Budget). It grew the same three front doors first: machine-plane API, CLI and MCP. Its pm/ specs are the model ours were grown from. Where the two apps are alike, keep the designs alike: same envelope shape, same error codes, same key mechanism and same write protocol.

## How we extend beyond the upstream baseline

Upstream ezBookkeeping ships three things we build on:
* A browser app with a JWT-authenticated `/api/v1`.
* Upstream API tokens. These are optional, full-access and off by default.
* A small HTTP MCP endpoint (`POST /mcp`, 7 tools). Optional and off by default.

Our fork adds seven things:
1. **Machine-plane API** at `/machine/v1`, inside the same Go server. It only answers loopback callers, and every call needs the API secret key (below). It calls the same `pkg/services` the browser's API uses, so the browser, the CLI and the agent never disagree about a number. Spec: `pm/apis.mdx`.
2. **CLI `ezbk`**: a thin Go client of the machine plane. It starts the app if it is down. Its flagship job is importing years of bank statements exactly once. Spec: `pm/cli.mdx`.
3. **MCP server `ezbookkeeping`** (tools prefixed `ezb_`): a thin Node + TypeScript stdio client of the machine plane, for Claude Code.
   * 75 tools: 50 read, 25 write. Writes are off by default. One tool deletes (`ezb_delete_transactions`, by id only), and it also needs the admin tier on both sides (`EZBKMCP_ALLOW_ADMIN=1`, `ezbk up --allow-admin`).
   * Spec: `pm/mcp.mdx`.
   * Its instructions to the model: `ai/mcp_prompt_ezbookkeeping.md`.
4. **More APIs, and more charting.** Upstream's statistics page groups categories and converts currencies in the browser (`src/stores/statistics.ts`). We move that work into Go analytics routes (`/machine/v1/analytics/*`) so every total is computed once. Future charts must read those same routes, never re-add numbers in new TypeScript (`pm/apis.mdx` §12.4).
5. **Statement ingest** on top of ezBookkeeping's own converters (OFX, QFX, QIF, CAMT, MT940, CSV and more). Upstream's importer has no duplicate detection, so the machine plane keeps its own `machine_import_record` table. On this fork, that table decides what counts as a duplicate.
6. **Undo journal.** ezBookkeeping has no undo, so the machine plane journals its own writes and `ezb_undo` / `ezbk undo` can reverse them.

7. **The error file.** Every fault from every runtime (browser, service worker, Go server, the binary's admin subcommands, `ezbk`, the MCP) is written once to `~/T/ezbookkeeping/error.err`, by `pkg/errfile` (Go, stdlib only; `cli/internal/errfile` is a byte-identical vendored copy) and `src/lib/errfile` (TypeScript, zero deps; `mcp/src/errfile` carries a drift-checked copy plus the node sink). Coverage is enforced, not assumed: the Go analyzer `scripts/errfilecheck` (run as `go vet -vettool`) and the ESLint rule `errfile/catch-must-report` fail the build on any silent error site, and `just check-errors` proves 100% file coverage. Spec: `pm/error_err.mdx`. When you write a `catch`, an `if err != nil` that swallows, a `recover()`, or a `go` statement, follow its §7 patterns — `just lint` will tell you if you did not.

Upstream's own API tokens (`[security] enable_api_token`) and its MCP endpoint (`[mcp] enable_mcp`) stay **off**. We neither use them nor modify them.

**Keep upstream merges cheap.** New code goes in new directories (`pkg/machine/`, `pkg/errfile/`, `cli/`, `mcp/`, `src/lib/errfile/`, `src/lib/export/`, `src/components/desktop/export/`, `scripts/`). The delta to upstream's files is deliberately small and fully listed:
* one `machine.Mount(router, config)` line and one `machine.Arm()` call in `cmd/webserver.go`,
* two lines in `src/views/desktop/transactions/ListPage.vue` (the import and the tag of the transaction list's More ▾ copy/download button, `pm/transaction_list.mdx` §9),
* the error-file nets of `pm/error_err.mdx` §8 — about 30 lines across `cmd/initializer.go`, `cmd/utility.go`, `pkg/log/logger.go` (`AddHook`), `pkg/middlewares/recovery.go`, `pkg/cron/cron_job.go`, `ezbookkeeping.go`, `src/desktop-main.ts`, `src/mobile-main.ts`, `src/sw.ts`, `src/lib/logger.ts`, `vite.config.ts`, `vitest.config.ts`, `eslint.config.mjs`,
* one added report line at each upstream error site that used to swallow its error (`pm/error_err.mdx` §17 — the operator decided complete fault coverage is worth this; a merge conflict at such a site is two lines with an obvious resolution: keep upstream's logic, re-apply the one call),
* the justfile.
Upstream sites that already log through `pkg/log` or `src/lib/logger.ts` are never edited: those two loggers are hooked once, so they report with zero call-site changes.

Anything that benefits every ezBookkeeping user, such as converter fixes or bug fixes, goes upstream as its own small PR.

## Bank Statements (private, outside this repo)

Here is where we read in the bank statements. Sometimes they start off as PDFs.
~/BGit/Bryan_git/Bryan_Arindom/bank_statements/

The directory below is a staging area where you can process and create bank statements. Read the PDFs recursively in the hierarchy under the parent bank_statements/ to find bank statements. They may be PDFs or other formats. Create them in whatever import format ezBookkeeping (or other software) likes to use — CSVs and its other supported import formats.
~/BGit/Bryan_git/Bryan_Arindom/bank_statements/import/

Prevent dupes. The bank statements might have dupes: multiple PDFs of the same month. Make sure the files in import/ remove any dupes, so the data is unified and not duplicated.

How the `ezbk` statements pipeline relates to that directory (`pm/apis.mdx` §14, `pm/cli.mdx` §10):
* `import/` is the **prepared archive**, and it is shared with the Actual Budget fork. The pipeline reads it in "prepared" mode, using its manifest and its OFX/CSV files. The pipeline itself never writes into `import/`.
* The pipeline writes its own derived files only to `{STATEMENTS_ROOT}/.ezbk-staging/`, which it git-ignores. That directory sits beside the Actual fork's `.actual-staging/`, and the two never touch each other.
* The code finds the statements root through configuration only: `ezbookkeeping.statements.root` in the credentials file, or `EZBK_STATEMENTS_DIR`. It is never a constant in code.

## Project Layout

The directory below holds the product management specification files on how everything is going to work.
~/BGit/Bryan_git/ezbookkeeping/pm/
* apis.mdx — the machine-plane API: the contract both clients use. Written first among equals.
* cli.mdx — `ezbk`
* mcp.mdx — the `ezbookkeeping` MCP server
* import_formats.mdx — the statement file formats, the converters, and what our pipeline writes
* category_analysis.mdx — the Statistics & Analysis page's Categorical Analysis tab (pie chart, drill-down)
* transaction_list.mdx — the Transaction List page, and the fork's More ▾ copy / download menu (CSV, YAML, Markdown)
pm/ holds no code, ever.

The directory below is where the source code goes for a CLI (command-line interface), so we can interface with this web app from the command line.
~/BGit/Bryan_git/ezbookkeeping/cli/
It is Go, in its own Go module, and imports nothing from the root module; the boundary is HTTP.

The directory below holds an MCP server. It is a Node TypeScript MCP that works with Claude Code, so from Claude Code we can interact with this web app.
~/BGit/Bryan_git/ezbookkeeping/mcp/
It has its own `package.json` (private, never published). It is not upstream's `pkg/mcp`, and it never extends that package.

This is the prompt file our MCP server uses to know about our APIs and how to interact with the user:
~/BGit/Bryan_git/ezbookkeeping/ai/mcp_prompt_ezbookkeeping.md

The machine-plane API goes in `~/BGit/Bryan_git/ezbookkeeping/pkg/machine/`, a single package that upstream has never heard of.

## Running it locally

* `just build` builds the Go binary `./ezbookkeeping`, the Vue front end in `./dist`, the CLI `./cli/bin/ezbk` and the MCP `./mcp/dist/index.js`, after syncing the vendored errfile copies. `just lint` runs go vet (plus the errfile analyzer) for both Go modules and the two ESLint runs; `just test` runs every suite; `just check-errors` runs the error-file coverage script; `just install-mcp yes` registers the MCP with Claude Code.
* `just run` (re)starts the server in the background at http://localhost:8080/, bound to 127.0.0.1, so `just build && just run` always serves the latest build.
  * It stops whichever ezBookkeeping server is running first, whether `just run`, `ezbk up` or a foreground run started it.
  * It waits until the server is healthy and checks that the machine plane is armed.
  * It shares `server.pid` and `server.log` with `ezbk up`/`ezbk stop`, so the two interoperate.
  * It refuses to kill a foreign process on the port, and refuses to start a second server on the same state dir.
  * Writes are on by default (`EZBK_MACHINE_ALLOW_WRITE=0 just run` turns them off); admin is off.
  * `just run-fg` runs in the foreground; `just stop`, `just status` and `just logs` do what they say.
  * The logic lives in `scripts/server.sh`.
  * Set `EBK_PORT` to change the port.
* `just dev` runs the Vite hot-reload server on :8081. It needs the server up (`just run`).
* Config lives in `conf/ezbookkeeping.ini`, which is upstream's file and is checked in.
  * Upstream overrides any key with `EBK_<SECTION>_<KEY>`.
  * `EBKCFP_<SECTION>_<KEY>` also works; it gives the path of a file holding the value.
  * Our own variables use the `EZBK_` prefix, so the two namespaces never collide.
* The machine plane's write and admin tiers are switched on at boot, never in the checked-in `.ini`:
  * `EZBK_MACHINE_ALLOW_WRITE=1` and `EZBK_MACHINE_ALLOW_ADMIN=1`, or
  * `ezbk up --allow-write`.
* Logs and state for our additions will live in `~/T/_ezbookkeeping/`:
  * `server.log` and `server.pid`
  * `cli.info` and `cli.err`
  * `mcp.info` and `mcp.err`
  * `machine.audit`
* Every fault from every runtime goes to **`~/T/ezbookkeeping/error.err`** (`EZBK_ERROR_FILE` overrides; `EZBK_ERROR_FILE_VERBOSE=1` also writes expected failures; `EZBK_ERROR_FILE_ECHO=1` echoes to stderr). `tail -f` that file first when anything misbehaves. The coverage report lands beside it as `error_file_coverage.json`.

## The API secret key

* It lives in `~/.credentials/ezbookkeeping.json` with mode 0600. This file is unique to this app. It is never shared with `actual_budget.json` or any other app.
* The web app mints the key itself on first boot:
  * 32 bytes from Go's `crypto/rand`, written as 64 hex characters.
  * That is longer than a UUID and has more than twice a UUIDv4's entropy.
  * Every later boot reuses the existing key.
* `ezbk` and the MCP read the same key and send it as the `X-Ezbk-Api-Key` header. If either runs before the server ever has, it mints the key with the same rules.
* This key is **not** `data/.secret_key`. That file is upstream's `[security] secret_key`, which encrypts 2FA secrets. Keep the two separate.
* Never log, print or commit the key. Only its fingerprint (e.g. `4f2a…/sha256:9c1b`) may appear.
* The key opens the books of one **bound user**:
  * the user named by `ezbookkeeping.machine.username`, or
  * the only user, if the install has exactly one.
  * When there are several users and none is named, never guess.

## MCP routing on this machine

Two MCP servers on this machine are about the operator's own money:
* `ezbookkeeping` (`ezb_` tools), for this app.
* `actual_budget` (`ab_` tools), for the sister fork.

Two others are not the operator's personal money:
* `quickbooks` is company bookkeeping.
* `act3` is filmmaking. Its "credits" are render credits, not money.

When a request does not say which personal-finance app is meant, ask once. Never answer from both, and never add their numbers together.

## Money rules (all three specs)

* **Amounts are integer hundredths**, never floats or decimal strings. ezBookkeeping keeps two decimal places for every currency.
* **Every amount carries its currency.** Never add or compare two currencies without converting them with the app's rates and naming those rates. The app keeps only the latest rates, not historical ones.
* **Transfers and opening balances are not income or spending.**
* **Clients never sum.** Totals come from the analytics routes.

## Private Data Boundary: Never Leak Into the Open Source Repo

These two directory hierarchies, and everything under them recursively, hold Bryan's very private personal and financial data:
* ~/BGit/Bryan_git/Bryan_Arindom/bank_statements/
* ~/BGit/Bryan_git/Bryan_Arindom/bank_statements/import/

The directory below is an open source project that we own and publish:
* ~/BGit/Bryan_git/ezbookkeeping/

HARD REQUIREMENT: Bryan's personal data must never end up anywhere in the ~/BGit/Bryan_git/ezbookkeeping/ hierarchy. No exceptions.
* Never copy, move, symlink, or write any file from the private directories above into ezbookkeeping/.
* Never put real data in code, tests, fixtures, sample files, docs, specs, logs, commit messages, or comments there. That includes account numbers, balances, transactions, payees, statement text, names, and addresses.
* When ezbookkeeping/ needs example data, make up synthetic data. Never derive it from the real statements. The specs use invented names such as `household`, `acme_llc`, `Northbank`, `Meridian`, `••4021` and user `operator`.
* Scripts in ezbookkeeping/ may read private files at runtime through a path the user supplies, but they must write their output outside that repo (for example, into bank_statements/import/), never inside it.
* Before committing anything in ezbookkeeping/, check the staged diff for private data. If anything looks real, stop and ask Bryan.
* Never let Chase statements, Fidelity statements, or any financial data get into the git repo. They will get into the database running on localhost, which never gets into the git repo.
* It is okay for the data to get into the database on localhost — just not into the git repo or this directory hierarchy, because it may accidentally get into the public repo.

### Runtime state location (watch this)

Upstream's `.ini` keeps runtime state inside this repo (`data/`, `log/`, `storage/`, all git-ignored). This fork does not use those paths: `just run` and `ezbk up` point the server at **`~/T/_ezbookkeeping/`** (`EZBK_STATE_DIR` overrides) through `EBK_DATABASE_DB_PATH`, `EBK_LOG_LOG_PATH`, `EBK_STORAGE_LOCAL_FILESYSTEM_PATH` and `EBKCFP_SECURITY_SECRET_KEY`:
* `~/T/_ezbookkeeping/data/ezbookkeeping.db` — the sqlite database (real financial data once statements are imported)
* `~/T/_ezbookkeeping/data/.secret_key` — upstream's `[security] secret_key` (not the API key)
* `~/T/_ezbookkeeping/log/ezbookkeeping.log` — logs, which can contain transaction details
* `~/T/_ezbookkeeping/storage/` — uploaded files (transaction pictures, avatars)

On first start both move a database or `.secret_key` an older version left in `./data/` out of the repo (never overwriting). Keep the `.gitignore` entries for `/data/`, `/log/`, `/storage/` anyway, never `git add -f` anything under them, and never start the server another way (a bare `./ezbookkeeping server run` would use the in-repo paths).

## Statement import formats

What a statement file must look like for this app, what each converter does with it, and what our pipeline writes (`*_ezbookkeeping.ofx` + a TSV companion + `manifest_ezbookkeeping.csv` with `file`, `opening_balance`, `opening_date`): `pm/import_formats.mdx`. The pipeline lives in the private archive at `~/BGit/Bryan_git/Bryan_Arindom/bank_statements/import/_tools/` and is driven by `~/BGit/Bryan_git/Bryan_Arindom/finances/prompts/p_ezbookkeeping_*.md`.

## Git

* Work on the current branch. Do not create branches or push unless asked.
* Use `git --no-pager diff`, never `git difftool`.
