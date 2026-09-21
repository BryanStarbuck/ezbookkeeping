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
  * NEVER hardcode that path into this prompt, into any spec, or into any source file. It is the
    operator's private financial archive and this repo is open source.
  * If it is not set, ask the operator for it once and write it into CREDENTIALS_FILE.

THE_PHASE_TO_BUILD is the phase identifier: P9
  * Use that value unless a different phase is already specified when this prompt is run.
  * Phases are defined in API_SPEC section 24 (the plane), CLI_SPEC section 22 (the CLI) and
    MCP_SPEC section 20 (the MCP). A plane phase drags its CLI verbs and its MCP tools with it.


Goal of this prompt = Build one phase of the machine-plane API into the Go server, prove it runs,
then bring the MCP and the CLI up onto it, and leave the specs and the status file true.

This is a BUILD AND RUN prompt, not a design prompt. The design is already written down. API_SPEC
is the contract, MCP_SPEC and CLI_SPEC are its two clients, ERROR_SPEC is the floor under all of
them, and MCP_PROMPT_FILE is what the model on the other end reads. Where this prompt and API_SPEC
disagree, API_SPEC is right.

Build one phase per run. A phase that is built, tested, run and reported is worth more than three
phases that are half written.

Create this file if it does not exist: THE_LOG_FILE

Create this file if it does not exist: STATUS_FILE


====================================================================
STAGE 0 — ORIENT BEFORE TOUCHING ANYTHING
====================================================================

* Read API_SPEC in full. It is long. Read it anyway; every later stage refers back to it by section
  number and guessing costs more than reading.

* Read the section of API_SPEC named "24. Build phases" and find the row for THE_PHASE_TO_BUILD.
  That row names what ships and what it depends on.

* Read STATUS_FILE. It is the record of what has actually been built, as opposed to what is
  specified. If a phase THE_PHASE_TO_BUILD depends on is not marked done there, STOP and say so:
    Output to stdout:
      "--------------------------------------------------------------"
      "BLOCKED: {THE_PHASE_TO_BUILD} depends on {phase}, which is not built."
      "Run this prompt with THE_PHASE_TO_BUILD = {phase} first."
      "--------------------------------------------------------------"

* Read what already exists, so nothing is rewritten that is already right:
  * Every file under PLANE_DIR. route.go holds RouteDef and Routes() — the ONE array the router,
    GET /capabilities and the tests are all built from.
  * APP_MOUNT_FILE, specifically the two lines that call machine.Mount and machine.Arm. Those are
    the only lines this fork adds to that file; do not add a third.
  * UPSTREAM_API_DIR and UPSTREAM_SERVICES_DIR — the upstream handlers and services every plane
    route reuses. A plane route calls the SAME service the browser's /api/v1 handler calls. It
    never re-implements a query and never re-adds a number.
  * ERRFILE_DIR and ERROR_SPEC sections 6 and 7 — every error site you write in this run must
    follow them, and `just check-errors` will fail the build if it does not.

* Confirm the toolchain works before writing code, not after:
  * cd {ROOT_DIR} && just build
  * If it fails, fix the build first and log what was wrong. A phase built on a broken build is a
    phase nobody can verify.


====================================================================
STAGE 1 — WRITE THE ROUTES
====================================================================

For each route in THE_PHASE_TO_BUILD, in the order API_SPEC lists them:

* Put the handler in the file API_SPEC section 4 says it goes in (routes_{family}.go). Do not
  invent a new file layout.

* Obey the eleven design rules in API_SPEC section 3. In particular:
  * One upstream service call per route. If a route seems to need two, either it is two routes or
    the service needs one new operation. A composed route is allowed ONLY if API_SPEC names it as
    one.
  * Money is an integer number of hundredths IN ONE NAMED CURRENCY in every argument and every
    field. No floats, no decimal strings, no adding two currencies without a named rate.
  * Absent is not zero.
  * Every write defaults to dry_run true, and the preview is the write with one boolean different.
    Never write a second function that predicts what the first one will do.
  * Every cap is reported with truncated and limit_applied.
  * Every error names a fix in its hint. An internal or upstream_error hint points at ERROR_FILE.

* Write the input validator by hand, one per route, in the file API_SPEC section 4 names. Unknown
  top-level fields are REJECTED, not ignored.

* Add the route to Routes() in route.go. There is no second list. If you find yourself adding a
  route name in two places, that is the bug.

* Every error site follows ERROR_SPEC section 7 (patterns G1 to G8): errfile.Caught where the
  original error would otherwise be lost, errfile.Expected where a failure is an answer, never a
  bare `_ = recover()`, never a bare `go func()`.

* Write the tests as you go, from the table in API_SPEC section 21. Do not batch the tests to the
  end of the phase.


====================================================================
STAGE 2 — BUILD AND RUN, AS NEEDED
====================================================================

This is the loop. Run it as many times as it takes. Do not report a route as done until it has
answered a real request on a running server.

* Build:
    cd {ROOT_DIR} && just build

* Start the server detached on a throwaway port with an ISOLATED work directory and an ISOLATED
  credentials file, so nothing in this loop touches the operator's books or the real key:
    EBK_WORK_DIR={tmp}/srv EBK_SERVER_HTTP_PORT=8099 EZBK_CREDENTIALS_FILE={tmp}/creds.json
    EZBK_STATE_DIR={tmp}/state EZBK_MACHINE_ALLOW_WRITE=1 ... ./ezbookkeeping server run
  (CLI_SPEC section 3 and cli/internal/bringup/bringup.go show the full environment the plane
  expects.)

* Confirm the plane is armed. The boot line says so; look for it in the server output:
    "Machine plane armed on /machine/v1 (key ..., writes ...)"
  * If it is missing, the plane did not initialise. Read the log and ERROR_FILE rather than
    guessing.

* Prove the gate ladder is intact before trusting any route result:
    * With the key: GET /machine/v1/ping returns ok true.
    * With no key: returns 401 with a constant body.
    * With a wrong key: the identical 401.
    * With Origin: https://evil.example and a VALID key: returns 404.
    * From a non-loopback peer: 404.
  * If any of those is wrong, STOP. Every later result is meaningless.

* Exercise each new route by hand with curl, reading the key from the isolated credentials file.
  Read the whole envelope, not just the status code. Check meta names the bound user you think it
  is.

* When a route is wrong, fix it and go back to the top of this stage. Build and run again.

* Run the tests:
    cd {ROOT_DIR} && go test ./pkg/machine/... ./pkg/errfile/...
    cd {ROOT_DIR} && go vet ./...
    cd {ROOT_DIR} && just check-errors

* Take the server down when the loop is finished and delete the isolated work directory.


====================================================================
STAGE 3 — BRING THE CLIENTS UP ONTO THE NEW ROUTES
====================================================================

A route with no caller is a route nobody notices is broken. Every phase ends with both clients able
to reach what it added.

* THE CLI, in CLI_DIR:
  * Add the verb CLI_SPEC section 6 names for each new route.
  * The CLI owns argument parsing, output formatting and bring-up, and owns no bookkeeping logic.
    It never re-filters, re-sums or rounds. The only conversion it is allowed is hundredths to a
    decimal string for --format table and --format csv, in exactly one function
    (cli/internal/render/money.go).
  * stdout carries the answer. Everything else is stderr.
  * CLI_DIR is its own Go module and imports nothing from the root module. It uses the vendored
    cli/internal/errfile, which `just sync-errfile` keeps byte-identical to ERRFILE_DIR.
  * cd {ROOT_DIR}/cli && go build ./... && go vet ./... && go test ./...

* THE MCP, in MCP_DIR:
  * Add the tool MCP_SPEC section 9 names for each new route. One tool, one route.
  * Write the description with all four mandatory clauses from MCP_SPEC section 9.2: what it does,
    what it costs, which sibling to use instead, and which server this is.
  * Add it to src/tools/registry.ts, the one array that tools/list and dispatch both read.
  * If the new route is a write, it needs the dry run, the confirm token and the ceiling. There is
    no write tool without all three, and it is off unless EZBKMCP_ALLOW_WRITE=1.
  * Nothing ever goes to stdout except JSON-RPC frames.
  * cd {ROOT_DIR}/mcp && npm run build && npm test && npm run lint

* MCP_PROMPT_FILE:
  * If the phase added a family the model would not otherwise know to reach for, add it to the
    playbooks. Keep the split: MCP_SPEC says which tools exist, MCP_PROMPT_FILE says when to reach
    for them and how to talk about the answer.
  * Every ezb_ tool name mentioned in MCP_PROMPT_FILE must exist. Check it.
  * Put NO private path, NO account name, NO entity name and NO amount in that file. It ships in a
    public repo.


====================================================================
STAGE 4 — PROVE IT AGAINST REAL DATA, WITHOUT TOUCHING REAL DATA
====================================================================

* Run the fixture suite first. Synthetic statements, invented banks, invented amounts. This is what
  CI runs and it must be green.

* Then, if and only if the operator asks for it, point the ingest routes at STATEMENTS_ROOT in READ
  mode only:
  * GET /machine/v1/ingest/manifest
  * POST /machine/v1/ingest/scan
  * POST /machine/v1/ingest/plan
  * Every one of those changes nothing. Report the counts.

* NEVER write into STATEMENTS_ROOT. It is audit evidence. The only directory this system writes
  under it is {STATEMENTS_ROOT}/.ezbk-staging/, and if that would collide with a directory the
  archive already owns, the route must refuse with conflict rather than stage over it.

* NEVER run an apply against the operator's real books without them asking in that run.


====================================================================
STAGE 5 — THE OPEN-SOURCE AND SAFETY CANARIES
====================================================================

Run these every phase, not just at the end. They are cheap and they catch the mistakes that are
expensive to unwind after a push.

* No secret in the tree:
    grep -rE "[0-9a-f]{64}" across PLANE_DIR, MCP_DIR/src, CLI_DIR and the built outputs

* No private path in the code:
    grep -r "/Users/" and the operator's entity names, across source, excluding fixtures and
    documentation comments

* No financial data in the repo:
    git status, and confirm nothing under the statements root, the staging directory, data/, log/
    or storage/ is tracked

* No float arithmetic on money:
    grep -r "float64\|ParseFloat\|parseFloat\|toFixed" across PLANE_DIR handlers, outside
    money.go, and across MCP_DIR/src outside the one rendering module

* No leak in a response:
    call every route in GET /machine/v1/capabilities against the fixture books and grep every
    response for the key pattern, for "secret", for "password" and for "/Users/"

* No silent fault:
    cd {ROOT_DIR} && just check-errors
    must report 0 violating files and 0 unwired runtimes

If any canary fails, fix it before the phase is reported done. A canary failure is not a warning.


====================================================================
STAGE 6 — UPDATE THE RECORD
====================================================================

* Update STATUS_FILE. One row per phase, using bracket notation so the state is scannable:
    [     ]  not started
    [IN-PR]  in progress
    [ DONE]  built, tested, run, and both clients reach it

  Row format:
    [ DONE]  P1  bound user, tier, validate, Routes(), /whoami /capabilities /health, passthrough
             routes: 5   tests: 14   cli verbs: 2   mcp tools: 4
             built: {Date}   notes: ...

* If the build revealed that API_SPEC is wrong, FIX API_SPEC. The spec is the contract and a spec
  that disagrees with the working code is worse than no spec. Say in the log what changed and why.
  Do not silently diverge.

* If a route turned out to need a calculation nobody anticipated, add it to API_SPEC as a route
  before adding it to a client as a calculation. That is the rule the whole architecture rests on.

* Update the coverage matrix in API_SPEC section 11 if the phase filled a cell.

* Append to THE_LOG_FILE: the phase, the routes built, the test counts, the canary results, and
  anything left undone.


====================================================================
STAGE 7 — COMMIT
====================================================================

* Follow the repo's own rules, which are not optional here:
  * Work on the current branch. Do not create, switch or push a branch.
  * Before staging, read the diff for private data: no account number, balance, payee, statement
    text, name or address anywhere. If anything looks real, STOP and ask the operator.
  * Never `git add -f` anything under data/, log/ or storage/.

* Run before committing:
    cd {ROOT_DIR} && go build ./... && go vet ./... && go test ./...
    cd {ROOT_DIR}/cli && go build ./... && go vet ./... && go test ./...
    cd {ROOT_DIR} && npm run lint && npm test
    cd {ROOT_DIR}/mcp && npm run build && npm test
    cd {ROOT_DIR} && just check-errors

* Then report:
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

* /machine/v1/ping returns 404 with a valid key.
  The plane was never armed. machine.Arm runs at boot, after the datastore is up, and if it could
  not resolve or mint a key it logs "machine plane NOT armed" and every route answers 404 on
  purpose (fail-closed). Read the boot log and ERROR_FILE.

* Every call returns 401 after a key change.
  The running server still holds the old key in memory. Restart it. This is deliberate; a plane
  that hot-reloads its own credential is a plane where a stolen key is revoked eventually.

* A test wrote a secret into the real home directory.
  Something resolved the key with mint=true outside Arm. Set EZBK_CREDENTIALS_FILE in the test and
  make sure the mint path is only reachable from machine.Arm and from the clients' bring-up.

* The write route answers write_disabled.
  The write tier is off. Start the server with EZBK_MACHINE_ALLOW_WRITE=1 (or ezbk up
  --allow-write). The MCP additionally needs EZBKMCP_ALLOW_WRITE=1. One switch is never enough on
  purpose.

* The route returns upstream_error and nothing is in the log.
  Before ERROR_SPEC, toFail dropped the original error. Now it is in ERROR_FILE with the request
  id; tail it. If it is not there, the site swallowed the error: `just check-errors` names the
  file.

* The server died and the terminal shows a goroutine stack.
  A `go func()` panicked without a net. ERROR_SPEC pattern G6: it must be errfile.Go(...).

* A plan says one number and the apply does another.
  Something is predicting what the importer will do instead of asking it. Delete that code. plan
  and apply are one call with one boolean different.

* The model reports a total nobody computed.
  A total was missing from the analytics family, so it summed rows. Add the route. Do not add the
  instruction; the instruction is already in MCP_PROMPT_FILE and prose does not beat a missing tool.

* The CLI build fails with an import of pkg/.
  CLI_DIR is its own module and imports nothing from the root module. The boundary is HTTP. The
  one shared piece of code, errfile, is vendored by `just sync-errfile`, never imported.
