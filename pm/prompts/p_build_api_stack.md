ROOT_DIR dir is ~/BGit/Bryan_git/ezbookkeeping
API_SPEC is file {ROOT_DIR}/pm/apis.mdx
MCP_SPEC is file {ROOT_DIR}/pm/mcp.mdx
CLI_SPEC is file {ROOT_DIR}/pm/cli.mdx
ERROR_SPEC is file {ROOT_DIR}/pm/error_err.mdx
MCP_PROMPT_FILE is file {ROOT_DIR}/ai/mcp_prompt_ezbookkeeping.md
PLANE_DIR dir is {ROOT_DIR}/pkg/machine
ERRFILE_DIR dir is {ROOT_DIR}/pkg/errfile
MCP_DIR dir is {ROOT_DIR}/mcp
CLI_DIR dir is {ROOT_DIR}/cli
APP_MOUNT_FILE is file {ROOT_DIR}/cmd/webserver.go
UPSTREAM_API_DIR dir is {ROOT_DIR}/pkg/api
UPSTREAM_SERVICES_DIR dir is {ROOT_DIR}/pkg/services
JUSTFILE is file {ROOT_DIR}/justfile
STATUS_FILE is file {ROOT_DIR}/pm/prompts/Build_Status.md
THE_DATE_TIME_STRING is the string "{Date}_{Time}_" where it uses "_" instead of any special characters, so it is purely alphanumeric or underscores.
THE_LOG_FILE is the file ~/T/_ezbookkeeping/{THE_DATE_TIME_STRING}_build_api_stack.log
ERROR_FILE is the file ~/T/ezbookkeeping/error.err
CREDENTIALS_FILE is file ~/.credentials/ezbookkeeping.json
STATEMENTS_ROOT is the directory named in CREDENTIALS_FILE under ezbookkeeping.statements.root
  * NEVER hardcode it into this prompt, any spec or any source file: it is the operator's private financial archive and this repo is open source.
  * If unset, ask the operator once and write it into CREDENTIALS_FILE.
THE_PHASE_TO_BUILD is the phase identifier: P9
  * Use it unless a different phase is given when this prompt is run.
  * Phases are defined in API_SPEC section 24 (the plane), CLI_SPEC section 22 (the CLI) and MCP_SPEC section 20 (the MCP). A plane phase drags its CLI verbs and MCP tools with it.

Goal = build one phase of the machine-plane API into the Go server, prove it runs, bring the MCP and CLI up onto it, and leave the specs and STATUS_FILE true.

This is BUILD AND RUN, not design. API_SPEC is the contract (it wins any disagreement with this prompt), MCP_SPEC and CLI_SPEC are its two clients, ERROR_SPEC is the floor under all of them, MCP_PROMPT_FILE is what the model on the other end reads. One phase per run: built, tested, run and reported beats three half written.

Create THE_LOG_FILE and STATUS_FILE if they do not exist.

====================================================================
STAGE 0 — ORIENT BEFORE TOUCHING ANYTHING
====================================================================

* Read API_SPEC in full (later stages cite it by section number). In "24. Build phases" find THE_PHASE_TO_BUILD's row: what ships and what it depends on.
* Read STATUS_FILE (what is actually built). If a dependency is not marked done, STOP:
    Output to stdout:
      "--------------------------------------------------------------"
      "BLOCKED: {THE_PHASE_TO_BUILD} depends on {phase}, which is not built."
      "Run this prompt with THE_PHASE_TO_BUILD = {phase} first."
      "--------------------------------------------------------------"
* Read what exists so nothing right gets rewritten:
  * Every file under PLANE_DIR. route.go holds RouteDef and Routes(): the ONE array the router, GET /capabilities and the tests are built from.
  * APP_MOUNT_FILE: only the two lines calling machine.Mount and machine.Arm belong to this fork; never add a third.
  * UPSTREAM_API_DIR and UPSTREAM_SERVICES_DIR: a plane route calls the SAME service the browser's /api/v1 handler calls; never re-implement a query or re-add a number.
  * ERRFILE_DIR and ERROR_SPEC sections 6 and 7: every error site this run writes must follow them (`just check-errors` fails the build otherwise).
* Confirm the toolchain first: cd {ROOT_DIR} && just build. If it fails, fix and log that before any phase work.

====================================================================
STAGE 1 — WRITE THE ROUTES
====================================================================

For each route of THE_PHASE_TO_BUILD, in API_SPEC order:
* Handler goes in the file API_SPEC section 4 names (routes_{family}.go); no new layout.
* Obey the eleven design rules of API_SPEC section 3, especially:
  * One upstream service call per route. Needing two means two routes or one new service operation; a composed route only if API_SPEC names it.
  * Money is an integer of hundredths IN ONE NAMED CURRENCY in every argument and field. No floats, no decimal strings, no adding currencies without a named rate.
  * Absent is not zero.
  * Every write defaults to dry_run true; the preview is the write with one boolean different. Never a second function predicting the first.
  * Every cap reports truncated and limit_applied.
  * Every error names a fix in its hint; internal and upstream_error hints point at ERROR_FILE.
* Hand-write one input validator per route, in the file section 4 names. Unknown top-level fields are REJECTED.
* Add the route to Routes() in route.go only. A route name in two places is the bug.
* Error sites follow ERROR_SPEC section 7 (G1 to G8): errfile.Caught where the original error would be lost, errfile.Expected where failure is an answer; never bare `_ = recover()` or bare `go func()`.
* Write tests as you go, from API_SPEC section 21's table; never batch them to the end.

====================================================================
STAGE 2 — BUILD AND RUN, AS NEEDED
====================================================================

Loop until done. No route is done until it answered a real request on a running server.
* Build: cd {ROOT_DIR} && just build
* Start detached on a throwaway port with ISOLATED work dir and credentials, so nothing touches the operator's books or real key:
    EBK_WORK_DIR={tmp}/srv EBK_SERVER_HTTP_PORT=8099 EZBK_CREDENTIALS_FILE={tmp}/creds.json
    EZBK_STATE_DIR={tmp}/state EZBK_MACHINE_ALLOW_WRITE=1 ... ./ezbookkeeping server run
  (Full environment: CLI_SPEC section 3 and cli/internal/bringup/bringup.go.)
* Boot output must show "Machine plane armed on /machine/v1 (key ..., writes ...)". If missing, read the log and ERROR_FILE; don't guess.
* Prove the gate ladder before trusting any result, else STOP:
    * With the key: GET /machine/v1/ping returns ok true.
    * No key: 401 with a constant body. Wrong key: the identical 401.
    * Origin: https://evil.example with a VALID key: 404.
    * Non-loopback peer: 404.
* curl each new route with the key from the isolated credentials file. Read the whole envelope, not just status; check meta names the bound user you expect.
* Wrong route: fix, then restart this stage.
* Tests:
    cd {ROOT_DIR} && go test ./pkg/machine/... ./pkg/errfile/...
    cd {ROOT_DIR} && go vet ./...
    cd {ROOT_DIR} && just check-errors
* When done, stop the server and delete the isolated work dir.

====================================================================
STAGE 3 — BRING THE CLIENTS UP ONTO THE NEW ROUTES
====================================================================

Every phase ends with both clients reaching what it added (an uncalled route breaks unnoticed).

* THE CLI, in CLI_DIR:
  * Add the verb CLI_SPEC section 6 names per new route.
  * CLI owns parsing, formatting and bring-up; no bookkeeping logic, never re-filters, re-sums or rounds. Its only conversion is hundredths to decimal string for --format table and --format csv, in exactly one function (cli/internal/render/money.go).
  * stdout carries the answer; everything else to stderr.
  * Own Go module, imports nothing from the root module; uses vendored cli/internal/errfile, kept byte-identical to ERRFILE_DIR by `just sync-errfile`.
  * cd {ROOT_DIR}/cli && go build ./... && go vet ./... && go test ./...
* THE MCP, in MCP_DIR:
  * Add the tool MCP_SPEC section 9 names per new route: one tool, one route.
  * Description has all four mandatory clauses of MCP_SPEC section 9.2: what it does, what it costs, which sibling to use instead, which server this is.
  * Add it to src/tools/registry.ts, the one array tools/list and dispatch both read.
  * A write tool needs dry run, confirm token and ceiling (no write tool without all three), off unless EZBKMCP_ALLOW_WRITE=1.
  * Nothing but JSON-RPC frames on stdout.
  * cd {ROOT_DIR}/mcp && npm run build && npm test && npm run lint
* MCP_PROMPT_FILE:
  * If the phase added a family the model wouldn't know to reach for, add it to the playbooks. Split: MCP_SPEC says which tools exist; MCP_PROMPT_FILE says when to use them and how to talk about answers.
  * Check every ezb_ tool name it mentions exists.
  * NO private path, account name, entity name or amount: it ships in a public repo.

====================================================================
STAGE 4 — PROVE IT AGAINST REAL DATA, WITHOUT TOUCHING REAL DATA
====================================================================

* Fixture suite first (synthetic statements, invented banks and amounts; what CI runs). Must be green.
* Only if the operator asks: point the ingest routes at STATEMENTS_ROOT, READ only, and report counts (none of these change anything):
  * GET /machine/v1/ingest/manifest
  * POST /machine/v1/ingest/scan
  * POST /machine/v1/ingest/plan
* NEVER write into STATEMENTS_ROOT (audit evidence). The only dir the system writes under it is {STATEMENTS_ROOT}/.ezbk-staging/; if that collides with a dir the archive owns, the route must refuse with conflict.
* NEVER run an apply against the operator's real books unless they ask in that run.

====================================================================
STAGE 5 — THE OPEN-SOURCE AND SAFETY CANARIES
====================================================================

Every phase. Any failure must be fixed before the phase is done; a canary failure is not a warning.
* No secret in the tree:
    grep -rE "[0-9a-f]{64}" across PLANE_DIR, MCP_DIR/src, CLI_DIR and the built outputs
* No private path in code:
    grep -r "/Users/" and the operator's entity names, across source, excluding fixtures and documentation comments
* No financial data in the repo:
    git status, and confirm nothing under the statements root, the staging directory, data/, log/ or storage/ is tracked
* No float arithmetic on money:
    grep -r "float64\|ParseFloat\|parseFloat\|toFixed" across PLANE_DIR handlers, outside money.go, and across MCP_DIR/src outside the one rendering module
* No leak in a response:
    call every route in GET /machine/v1/capabilities against the fixture books and grep every response for the key pattern, for "secret", for "password" and for "/Users/"
* No silent fault:
    cd {ROOT_DIR} && just check-errors
    must report 0 violating files and 0 unwired runtimes

====================================================================
STAGE 6 — UPDATE THE RECORD
====================================================================

* STATUS_FILE: one row per phase, bracket notation:
    [     ]  not started
    [IN-PR]  in progress
    [ DONE]  built, tested, run, and both clients reach it

  Row format:
    [ DONE]  P1  bound user, tier, validate, Routes(), /whoami /capabilities /health, passthrough
             routes: 5   tests: 14   cli verbs: 2   mcp tools: 4
             built: {Date}   notes: ...
* If the build showed API_SPEC is wrong, FIX API_SPEC and log what changed and why; never silently diverge.
* An unanticipated calculation becomes an API_SPEC route before it becomes client-side calculation (the rule the architecture rests on).
* Update API_SPEC section 11's coverage matrix if the phase filled a cell.
* Append to THE_LOG_FILE: phase, routes built, test counts, canary results, anything left undone.

====================================================================
STAGE 7 — COMMIT
====================================================================

* Repo rules, not optional:
  * Current branch only; never create, switch or push a branch.
  * Before staging, read the diff for private data (account number, balance, payee, statement text, name, address). If anything looks real, STOP and ask the operator.
  * Never `git add -f` anything under data/, log/ or storage/.
* Before committing:
    cd {ROOT_DIR} && go build ./... && go vet ./... && go test ./...
    cd {ROOT_DIR}/cli && go build ./... && go vet ./... && go test ./...
    cd {ROOT_DIR} && npm run lint && npm test
    cd {ROOT_DIR}/mcp && npm run build && npm test
    cd {ROOT_DIR} && just check-errors
* Then:
    Output to stdout:
      "=============================================================="
      "PHASE {THE_PHASE_TO_BUILD} COMPLETE"
      "  routes built     : {n}"
      "  tests added      : {n}"
      "  cli verbs        : {n}"
      "  mcp tools        : {n}"
      "  canaries         : all green"
      "  next phase       : {next}"
      "=============================================================="

====================================================================
THE THINGS THAT GO WRONG, AND WHAT THEY LOOK LIKE
====================================================================

* /machine/v1/ping returns 404 with a valid key: plane never armed. machine.Arm runs at boot after the datastore; if it can't resolve or mint a key it logs "machine plane NOT armed" and every route 404s (fail-closed). Read the boot log and ERROR_FILE.
* Every call 401s after a key change: the server holds the old key in memory; restart it (deliberate: no hot-reload of its own credential).
* A test wrote a secret into the real home dir: something resolved the key with mint=true outside Arm. Set EZBK_CREDENTIALS_FILE in the test; mint must be reachable only from machine.Arm and the clients' bring-up.
* Write route answers write_disabled: start the server with EZBK_MACHINE_ALLOW_WRITE=1 (or ezbk up --allow-write); the MCP also needs EZBKMCP_ALLOW_WRITE=1. Two switches on purpose.
* upstream_error with nothing in the log: the error should be in ERROR_FILE with the request id; tail it. If absent, the site swallowed it and `just check-errors` names the file.
* Server died with a goroutine stack: a bare `go func()` panicked; ERROR_SPEC G6 says it must be errfile.Go(...).
* Plan and apply disagree: some code predicts the importer instead of asking it. Delete it; plan and apply are one call with one boolean different.
* The model reports a total nobody computed: the analytics family lacked that total, so it summed rows. Add the route; prose in MCP_PROMPT_FILE does not beat a missing tool.
* CLI build fails importing pkg/: CLI_DIR is its own module; the boundary is HTTP. errfile is vendored by `just sync-errfile`, never imported.
