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

[ DONE]  P1-P9 of pm/mcp.mdx §20 — built in one pass by builder agent B3 of the error-file
              rollout (pm/error_err.mdx §16.2), against the errfile API from the start.
              65 tools (47 read, 18 write; none deletes), the six gates, credentials vectors,
              the twelve canaries of §7.0, instructions pipeline from
              ai/mcp_prompt_ezbookkeeping.md, audit line, node error sink (§16.3).
              started : 2026-09-21
              built   : 2026-09-21
              tests   : 22 files, 158 tests, all green (vitest: catalogue, gates, credentials
                        vectors, instructions freshness, canary parity, the 12 canaries, the
                        vendored errfile suite, and the LIVE integration suite against a real
                        server on an ephemeral port with temp SQLite and synthetic books)
              verified: hand-piped initialize over stdio (serverInfo.name ezbookkeeping,
                        capabilities {tools:{}}, instructions first sentence = §3.4 layer 5);
                        dist/ grep clean of console.log / process.stdout.write; no fetch(,
                        no child_process, one socket opener; write tier off -> all 18 listed
                        and refusing with both switches named; on -> confirm_required, stale
                        token conflict, ceiling too_many_changes with the real count;
                        plan twice identical, apply then plan -> 0 new; undo after every write
                        restores the row hash. Where pm/mcp.mdx disagreed with pkg/machine the
                        builder followed the plane; the spec was reconciled 2026-09-21 (Phase D).
              register: just install-mcp yes   (prints and runs the `claude mcp add` line)

--------------------------------------------------------------------------------------------------
THE ERROR FILE  (pkg/errfile, src/lib/errfile, ~/T/ezbookkeeping/error.err — pm/error_err.mdx)
--------------------------------------------------------------------------------------------------

[ DONE]  A    the spec pm/error_err.mdx and the cross-language vectors
              pkg/errfile/testdata/vectors.json        2026-09-21
[ DONE]  B1   Go library pkg/errfile + pkg/errfile/server, vendored cli/internal/errfile, the
              server nets (recovery middleware, machine-plane wrap, cron, main), the
              scripts/errfilecheck analyzer, justfile recipes            2026-09-21
[ DONE]  B2   TS library src/lib/errfile (browser sink + node sink in mcp/src/errfile), the web
              nets, the eslint errfile rule, scripts/error-file-coverage.mjs   2026-09-21
[ DONE]  B3   the MCP server with N14 — see THE MCP SERVER above            2026-09-21
[ DONE]  C    twelve partition agents over pkg/, cmd/, cli/, src/, mcp/: 629 violations -> 0
              across 199 files                                              2026-09-21
[ DONE]  D    eslint rule flipped to "error"; coverage report 875 files, 0 violating,
              0 unwired; every suite green (root Go, cli Go, vitest, mcp vitest); the four
              PM specs reconciled to the code (pm/apis.mdx, pm/cli.mdx, pm/mcp.mdx, this
              file)                                                        2026-09-21

--------------------------------------------------------------------------------------------------
STATEMENT IMPORT FORMATS  (pm/import_formats.mdx — the contract for every file the ingest plane reads)
--------------------------------------------------------------------------------------------------

[ DONE]  S1   pm/import_formats.mdx: 15 sections mirroring the Actual fork's, every format read from
              the converters (OFX/QFX, native CSV/TSV/JSON, QIF, CAMT, MT940, IIF, GnuCash,
              Beancount, Firefly III, custom, regional), probed on synthetic files; 20 upstream
              defects recorded                                              2026-09-21
[ DONE]  S2   plane: manifest `file` (one file per account in a shared directory),
              `opening_balance` + `opening_date` (account created with its one Balance
              Modification at 11:59:59 UTC), kind aliases, …manifest_ezbookkeeping.csv found before
              import/accounts.csv, empty statements are not conflicts; tests
              TestIngManifestFileOpeningAndKindAliases, TestIngOpeningTime,
              TestIngEmptyOfxIsNotAConflict                                 2026-09-21
[ DONE]  S3   upstream fix: the SGML decoder no longer loops forever on a bare & or < (DoS);
              TestSGMLDecoderDecode_BareAmpersandReturnsErrorInsteadOfLooping — upstream PR candidate
                                                                            2026-09-21
[ DONE]  S4   runtime state out of the repo: `ezbk up` (RuntimeDirs) and `just run` use
              ~/T/_ezbookkeeping/{data,log,storage} and move a stray ./data/ db once  2026-09-21
[ DONE]  S5   MCP: instructions (which manifest, fallback categories per row mode, opening balances,
              the import playbook), ezb_plan_accounts text; 159 tests green  2026-09-21
[ DONE]  S6   rehearsal: the private archive's personal tree imported into a THROWAWAY server through
              the MCP — every account's balance equal to its last printed statement, every re-plan
              new 0, a second full run added nothing; throwaway database deleted   2026-09-21

[ DONE]  S7   real import through the MCP into the operator's own sign-in user: every account's
              balance equal to its last printed statement, every re-plan new 0, a second full run
              applied nothing; write tier turned back off afterwards      2026-09-21

[ DONE]  S8   own-account moves as transfers (apis.mdx §10.3.2): POST /transactions/transfer-candidates
              (read: same currency and amount, ≤ window_days, last-four or transfer-word hint,
              unambiguous pairs apart from ambiguous groups) and POST /transactions/convert-to-transfer
              (write: pairs → one transfer, single rows → a transfer against a counterpart account;
              per-account balance assertion; import records re-pointed so re-plans stay at new 0;
              undo op txn.unconvert). MCP ezb_find_transfer_pairs, ezb_convert_to_transfer and the
              one admin tool ezb_delete_transactions (EZBKMCP_ALLOW_ADMIN; the plane admits the MCP to
              DELETE /transactions/bulk only) — 72 tools, 49 read, 23 write; CLI `ezbk transactions
              transfer-candidates | convert-to-transfer`. Tests TestXfer* (end to end through upstream's
              handlers on a temp SQLite, incl. a real ingEvaluate re-plan), TestGatesMcpReachesOnlyTheBulkDelete,
              TestXf* (cli), mcp/test/transfers.test.ts                    2026-09-21

NEXT: the confirm-group account imports only when the operator names it (--group confirm).

--------------------------------------------------------------------------------------------------
KNOWN
--------------------------------------------------------------------------------------------------
  * Upstream's own API tokens ([security] enable_api_token) and upstream's MCP endpoint
    ([mcp] enable_mcp) stay OFF. This fork neither uses nor modifies them (CLAUDE.md).
  * The runtime state lives in ~/T/_ezbookkeeping/ (CLAUDE.md "Runtime state location"); only a
    bare `./ezbookkeeping server run` would still use the in-repo paths — never start it that way.
