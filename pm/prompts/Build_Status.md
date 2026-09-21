EZBOOKKEEPING — MACHINE-PLANE, CLI, MCP AND ERROR-FILE BUILD STATUS

What has actually been BUILT, as opposed to what is specified. Plane phases are defined in
pm/apis.mdx section 24, CLI phases in pm/cli.mdx section 22, MCP phases in pm/mcp.mdx section 20,
and the error-file rollout in pm/error_err.mdx section 16. Updated by
pm/prompts/p_build_api_stack.md stage 6 and by the error-file rollout.

Bracket key:
  [     ]  not started
  [IN-PR]  in progress
  [ DONE]  built, tested, run on a live server, and both clients reach it

--------------------------------------------------------------------------------------------------
THE MACHINE PLANE  (pkg/machine, mounted by two lines in cmd/webserver.go)
--------------------------------------------------------------------------------------------------

[ DONE]  P0   mount, key (mint/merge/resolve, ~/.credentials/ezbookkeeping.json 0600, flock'd
              compare-and-set), gates 1-3 (real socket peer, Origin/Sec-Fetch, Host pin,
              constant-time key compare), envelope, /ping, fail-closed, the mount-line test
[ DONE]  P1   bound user, tier gate, validate, Routes() (the one array), /whoami /capabilities
              /health, the passthrough (§16)
[ DONE]  P2   typed read families: accounts, transactions (cursor walk), categories, tags,
              tag groups, templates/schedules, exchange rates, insights, data statistics/export
[ DONE]  P3   analytics plane (§12): per-currency totals, named-rate conversion, detectors;
              parity test against src/stores/statistics.ts
[ DONE]  P4   confirm.go, the write protocol (dry run default, confirm token, ceiling, audit),
              the journal; transactions add/modify, set-category, tags
[ DONE]  P5   undo / redo
[ DONE]  P6   remaining writes: accounts, categories, tags, templates/schedules, rates,
              reconcile, batch
[ DONE]  P7   ingest plane, prepared mode: manifest, upstream converters, both dedupe layers,
              machine_import_record, maps, plan/apply, file import, deletions respected
[ DONE]  P8   account provisioning (§15) and raw-mode extraction
[ DONE]  P9   admin tier, pictures and custom icons, NDJSON progress, canary suite,
              upstream-drift fixtures
              built : 2026-09-21 (commit f68f7fb7)
              verified: go build/vet/test (upstream suite + pkg/machine) green; live end-to-end
                     run of every route against an isolated server with synthetic data

--------------------------------------------------------------------------------------------------
THE CLI  (cli/, its own Go module, stdlib only — `ezbk`, 141 verbs)
--------------------------------------------------------------------------------------------------

[ DONE]  all phases of pm/cli.mdx §22: bring-up, key, doctor, reads, analytics, the statements
              pipeline, writes, undo, raw, admin; json/table/csv output; the exit-code contract
              built : 2026-09-21 (commit f68f7fb7)
              verified: cli go vet/test green; every verb run live against the isolated server

--------------------------------------------------------------------------------------------------
THE MCP SERVER  (mcp/, Node + TypeScript, stdio, `ezb_` tools)
--------------------------------------------------------------------------------------------------

[IN-PR]  P1-P9 of pm/mcp.mdx §20 — being built in one pass by builder agent B3 of the error-file
              rollout (pm/error_err.mdx §16.2), against the errfile API from the start.
              65 tools (47 read, 18 write), the six gates, credentials vectors, canaries,
              instructions pipeline from ai/mcp_prompt_ezbookkeeping.md, audit line.
              started: 2026-09-21

--------------------------------------------------------------------------------------------------
THE ERROR FILE  (pkg/errfile, src/lib/errfile, ~/T/ezbookkeeping/error.err — pm/error_err.mdx)
--------------------------------------------------------------------------------------------------

[ DONE]  A    the spec pm/error_err.mdx and the cross-language vectors
              pkg/errfile/testdata/vectors.json        2026-09-21
[IN-PR]  B1   Go library pkg/errfile + pkg/errfile/server, vendored cli/internal/errfile,
              nets N5 N7 N8 N9 N10 N11 N13, scripts/errfilecheck analyzer, justfile recipes
[IN-PR]  B2   TS library src/lib/errfile, node sink in mcp/src/errfile, nets N1 N2 N3 N4 N15,
              ESLint rule, scripts/error-file-coverage.mjs
[IN-PR]  B3   the MCP server with N14 (see above)
[     ]  C    twelve partitioned retrofit agents (every violating file in pkg/, cmd/, cli/,
              src/, mcp/)
[     ]  D    rule flipped to "error", coverage 100%, private-data check, spec/CLAUDE.md updates

NEXT: finish B1-B3, run scripts/error-file-coverage.mjs --partition 12, run Phase C, close with D.

--------------------------------------------------------------------------------------------------
KNOWN
--------------------------------------------------------------------------------------------------
  * Upstream's own API tokens ([security] enable_api_token) and upstream's MCP endpoint
    ([mcp] enable_mcp) stay OFF. This fork neither uses nor modifies them (CLAUDE.md).
  * The runtime state (data/, log/, storage/) still lives inside the repo, git-ignored. Point
    db_path/log_path/storage at ~/T/ before importing real statements (CLAUDE.md, "Runtime state
    location").
