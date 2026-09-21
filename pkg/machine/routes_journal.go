package machine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/datastore"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/log"
)

// routes_journal.go — undo · redo · journal · batch (apis.mdx §9.4, §9.6, §10.9).
//
// ezBookkeeping has no undo stack, so every machine-plane write records its own inverse in
// machine_journal (journal.go). /undo runs the inverse of the most recent un-undone entry of the
// bound user, in REVERSE op order, through the executors the families registered with
// RegisterInverse; each executor refuses with a conflict when its row moved since the write. /redo
// re-applies an undone entry through each op's Redo. /batch applies an ordered list of write
// operations under one confirm token, one ceiling and ONE journal entry, rolling back through the
// journal when an operation fails.

func init() {
	registerRoutes(jrRoutes)
}

func jrRoutes() []RouteDef {
	return []RouteDef{
		{
			Method: "POST", Path: "/undo", Tier: TierWrite, Handler: jrHandleUndo,
			Summary:   "Undo the most recent machine-plane write of the bound user (no dry_run: it returns what it reversed). Machine-plane writes only; a browser edit cannot be undone, and undo refuses with conflict when a row it would touch was edited since. Body: {journal_id?}.",
			Untrusted: []string{"summary"},
			Features:  []string{"undo"},
		},
		{
			Method: "POST", Path: "/redo", Tier: TierWrite, Handler: jrHandleRedo,
			Summary:   "Re-apply the most recently undone machine-plane write, available until a newer write lands. Body: {journal_id?}.",
			Untrusted: []string{"summary"},
			Features:  []string{"undo"},
		},
		{
			Method: "GET", Path: "/journal", Tier: TierRead, Handler: jrHandleJournal,
			Summary:   "Recent machine-plane writes of the bound user: route, when, change counts, client, summary, undone? Never amounts. Args: limit, offset, include_undone.",
			Untrusted: []string{"summary"},
			Features:  []string{"undo"},
		},
		{
			Method: "POST", Path: "/batch", Tier: TierWrite, DryRunnable: true, Composed: true, Handler: jrHandleBatch,
			Summary:   "Apply an ordered list of write operations under one confirm token, one ceiling (counting changes, not operations) and one journal entry; on the first failure the applied ones are reversed through the journal and the response says rolledBack. Body: {operations: [{op: \"METHOD /path\", params?, query?, args?}], dry_run, confirm_token, max_changes}. GET /batch/ops lists the closed op set.",
			Untrusted: []string{"preview"},
			Features:  []string{"batch"},
		},
		{
			Method: "GET", Path: "/batch/ops", Tier: TierRead, Handler: jrHandleBatchOps,
			Summary:  "The closed set of operations POST /batch accepts (every live, dry-runnable write-tier route).",
			Features: []string{"batch"},
		},
	}
}

// jrMu serialises undo, redo and batch applies, so two of them never interleave their inverse ops
var jrMu sync.Mutex

// jrMaxBatchOps bounds one batch; the change ceiling is the real limit, this stops a runaway body
const jrMaxBatchOps = 1000

// jrUndoNote is said wherever undo is described, so no caller believes it reaches browser edits
const jrUndoNote = "undo reverses machine-plane writes only; edits made in the browser are not in the journal"

// ---------------------------------------------------------------------------------------------
// journal storage
// ---------------------------------------------------------------------------------------------

func jrDB(mc *Ctx) (*datastore.Database, error) {
	if datastore.Container == nil || datastore.Container.UserDataStore == nil {
		return nil, NewFail(CodeNotReady, "wait for the server to finish starting", "the database is not open")
	}

	return datastore.Container.UserDataStore.Choose(mc.Uid), nil
}

func jrDecodeOps(entry *MachineJournal) ([]InverseOp, error) {
	var ops []InverseOp

	if strings.TrimSpace(entry.Inverse) == "" {
		return ops, nil
	}

	if err := json.Unmarshal([]byte(entry.Inverse), &ops); err != nil {
		errfile.Caught("decoding the journal entry's inverse operations", err, errfile.F("journal_id", entry.JournalId))
		return nil, NewFail(CodeInternal, "the journal entry is unreadable; it cannot be undone automatically (GET /machine/v1/journal shows it)", "journal entry %d has a malformed inverse", entry.JournalId)
	}

	return ops, nil
}

// jrLatestActive returns the most recent un-undone entry of the bound user, or nil
func jrLatestActive(mc *Ctx) (*MachineJournal, error) {
	db, err := jrDB(mc)

	if err != nil {
		return nil, err
	}

	var rows []*MachineJournal

	if err := db.NewSession(mc.Web).Where("uid=? AND undone=?", mc.Uid, false).OrderBy("journal_id desc").Limit(1).Find(&rows); err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return nil, nil
	}

	return rows[0], nil
}

// jrRedoCandidate returns the entry /redo would re-apply, or nil. Undo always reverses the newest
// active entry, so the undone entries above the newest active one form the redo stack and the
// lowest of them was undone last. Undone entries below an active one were closed by a newer write.
func jrRedoCandidate(mc *Ctx) (*MachineJournal, error) {
	db, err := jrDB(mc)

	if err != nil {
		return nil, err
	}

	var floor int64

	if active, err := jrLatestActive(mc); err != nil {
		return nil, err
	} else if active != nil {
		floor = active.JournalId
	}

	var rows []*MachineJournal

	if err := db.NewSession(mc.Web).Where("uid=? AND undone=? AND journal_id>?", mc.Uid, true, floor).OrderBy("journal_id asc").Limit(1).Find(&rows); err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return nil, nil
	}

	return rows[0], nil
}

func jrGetEntry(mc *Ctx, id int64) (*MachineJournal, error) {
	db, err := jrDB(mc)

	if err != nil {
		return nil, err
	}

	var rows []*MachineJournal

	if err := db.NewSession(mc.Web).Where("uid=? AND journal_id=?", mc.Uid, id).Limit(1).Find(&rows); err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return nil, nil
	}

	return rows[0], nil
}

// jrMarkUndone flips an entry's undone flag as a compare-and-set, so two racing undos cannot both
// claim the same entry
func jrMarkUndone(mc *Ctx, id int64, undone bool) error {
	db, err := jrDB(mc)

	if err != nil {
		return err
	}

	bean := &MachineJournal{Undone: undone}

	if undone {
		bean.UndoneUnixTime = time.Now().Unix()
	}

	n, err := db.NewSession(mc.Web).ID(id).Cols("undone", "undone_unix_time").Where("uid=? AND undone=?", mc.Uid, !undone).Update(bean)

	if err != nil {
		return err
	}

	if n < 1 {
		return Conflict("GET /machine/v1/journal shows the current state; retry", "journal entry %d changed state while it was being processed", id)
	}

	return nil
}

func jrDeleteEntries(mc *Ctx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	db, err := jrDB(mc)

	if err != nil {
		return err
	}

	_, err = db.NewSession(mc.Web).Where("uid=?", mc.Uid).In("journal_id", ids).Delete(new(MachineJournal))

	return err
}

// ---------------------------------------------------------------------------------------------
// running inverse and redo ops
// ---------------------------------------------------------------------------------------------

// jrExec runs one executor, turning a panic into an internal Fail so a half-built executor can
// never take the server down mid-undo
func jrExec(mc *Ctx, fn InverseExecutor, op InverseOp) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Errorf(mc.Web, "[machine.undo] executor %q panicked: %v", op.Kind, rec)
			err = NewFail(CodeInternal, "read ~/T/ezbookkeeping/error.err for the server-side detail", "the %s step failed", op.Kind)
		}
	}()

	return fn(mc, op.Payload, op.Check)
}

// jrPreflightUndo checks every op has a registered executor before anything is touched
func jrPreflightUndo(ops []InverseOp) error {
	var missing []string

	for _, op := range ops {
		if _, ok := LookupInverse(op.Kind); !ok {
			missing = append(missing, op.Kind)
		}
	}

	if len(missing) > 0 {
		return NewFail(CodeInternal, "this build cannot reverse that write; reverse it by hand in the web UI (GET /machine/v1/journal shows it)", "no undo executor is registered for %s", strings.Join(jrUnique(missing), ", ")).WithDetails(map[string]any{"missingKinds": jrUnique(missing)})
	}

	return nil
}

// jrPreflightRedo checks every op carries a registered redo step
func jrPreflightRedo(ops []InverseOp) error {
	var missing []string

	for _, op := range ops {
		if op.Redo == nil {
			missing = append(missing, op.Kind+" (no redo recorded)")
			continue
		}

		if _, ok := LookupInverse(op.Redo.Kind); !ok {
			missing = append(missing, op.Redo.Kind)
		}
	}

	if len(missing) > 0 {
		return Conflict("this write cannot be redone automatically; make the change again through its own route", "the entry has steps that cannot be redone: %s", strings.Join(jrUnique(missing), ", ")).WithDetails(map[string]any{"notRedoable": jrUnique(missing)})
	}

	return nil
}

// jrRunUndoOps applies ops' inverses in REVERSE order. If step k fails after earlier steps ran, the
// steps already reversed are re-applied through their Redo (best effort) so the books are left as
// they were before the undo, and the failure is reported with what the restore managed.
func jrRunUndoOps(mc *Ctx, ops []InverseOp) (int, error) {
	if err := jrPreflightUndo(ops); err != nil {
		return 0, err
	}

	var done []InverseOp

	for i := len(ops) - 1; i >= 0; i-- {
		fn, _ := LookupInverse(ops[i].Kind)

		if err := jrExec(mc, fn, ops[i]); err != nil {
			restoreErrs := jrRestoreForward(mc, done)
			f := toFail(err)
			details := map[string]any{"failedStep": i, "kind": ops[i].Kind, "stepsReversedBeforeFailure": len(done), "restored": len(restoreErrs) == 0}

			if len(restoreErrs) > 0 {
				details["restoreErrors"] = restoreErrs
				f.Hint = f.Hint + "; part of the undo could not be put back — GET /machine/v1/journal and check the rows named in details"
			}

			return len(done), jrWithDetails(f, details)
		}

		done = append(done, ops[i])
	}

	return len(done), nil
}

// jrRestoreForward re-applies (through Redo) steps an interrupted undo already reversed, newest
// reversal first
func jrRestoreForward(mc *Ctx, reversed []InverseOp) []string {
	var errsOut []string

	for i := len(reversed) - 1; i >= 0; i-- {
		op := reversed[i]

		if op.Redo == nil {
			errsOut = append(errsOut, fmt.Sprintf("%s: no redo recorded", op.Kind))
			continue
		}

		fn, ok := LookupInverse(op.Redo.Kind)

		if !ok {
			errsOut = append(errsOut, fmt.Sprintf("%s: no executor", op.Redo.Kind))
			continue
		}

		if err := jrExec(mc, fn, *op.Redo); err != nil {
			errsOut = append(errsOut, fmt.Sprintf("%s: %s", op.Redo.Kind, toFail(err).Message))
		}
	}

	return errsOut
}

// jrRunRedoOps applies ops' Redo in FORWARD order; on failure the steps already redone are undone
// again (reverse order) so a redo is all or nothing as far as the executors allow
func jrRunRedoOps(mc *Ctx, ops []InverseOp) (int, error) {
	if err := jrPreflightRedo(ops); err != nil {
		return 0, err
	}

	var done []InverseOp

	for i, op := range ops {
		fn, _ := LookupInverse(op.Redo.Kind)

		if err := jrExec(mc, fn, *op.Redo); err != nil {
			var restoreErrs []string

			for j := len(done) - 1; j >= 0; j-- {
				ufn, _ := LookupInverse(done[j].Kind)

				if uerr := jrExec(mc, ufn, done[j]); uerr != nil {
					restoreErrs = append(restoreErrs, fmt.Sprintf("%s: %s", done[j].Kind, toFail(uerr).Message))
				}
			}

			f := toFail(err)
			details := map[string]any{"failedStep": i, "kind": op.Redo.Kind, "stepsRedoneBeforeFailure": len(done), "restored": len(restoreErrs) == 0}

			if len(restoreErrs) > 0 {
				details["restoreErrors"] = restoreErrs
			}

			return len(done), jrWithDetails(f, details)
		}

		done = append(done, op)
	}

	return len(done), nil
}

func jrWithDetails(f *Fail, extra map[string]any) *Fail {
	out := *f
	merged := map[string]any{}

	if d, ok := f.Details.(map[string]any); ok {
		for k, v := range d {
			merged[k] = v
		}
	} else if f.Details != nil {
		merged["cause"] = f.Details
	}

	for k, v := range extra {
		merged[k] = v
	}

	out.Details = merged

	return &out
}

func jrUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string

	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}

	return out
}

func jrOpKinds(ops []InverseOp) []string {
	kinds := make([]string, 0, len(ops))

	for _, op := range ops {
		kinds = append(kinds, op.Kind)
	}

	kinds = jrUnique(kinds)
	sort.Strings(kinds)

	return kinds
}

// ---------------------------------------------------------------------------------------------
// POST /undo · POST /redo
// ---------------------------------------------------------------------------------------------

type jrUndoRequest struct {
	// JournalId, when given, is a guard: act only if that entry is the one next in line. Retrying an
	// undo that already happened then answers undone:false instead of undoing the next entry.
	JournalId string `json:"journal_id,omitempty"`
}

func jrEntryView(entry *MachineJournal, ops []InverseOp) map[string]any {
	view := map[string]any{
		"journalId": idString(entry.JournalId),
		"route":     entry.Route,
		"createdAt": time.Unix(entry.CreatedUnixTime, 0).UTC().Format(time.RFC3339),
		"changes":   entry.Changes,
		"client":    entry.Client,
		"summary":   entry.Summary,
		"undone":    entry.Undone,
		"steps":     len(ops),
		"stepKinds": jrOpKinds(ops),
	}

	if entry.Undone && entry.UndoneUnixTime > 0 {
		view["undoneAt"] = time.Unix(entry.UndoneUnixTime, 0).UTC().Format(time.RFC3339)
	} else {
		view["undoneAt"] = nil
	}

	redoable := len(ops) > 0

	for _, op := range ops {
		if op.Redo == nil {
			redoable = false
			break
		}
	}

	view["redoable"] = redoable

	return view
}

func jrHandleUndo(mc *Ctx) (any, error) {
	var req jrUndoRequest

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if err := mc.RequireWriteTier(); err != nil {
		return nil, err
	}

	var guard int64

	if strings.TrimSpace(req.JournalId) != "" {
		id, err := ResolveId("journal_id", req.JournalId)

		if err != nil {
			return nil, err
		}

		guard = id
	}

	jrMu.Lock()
	defer jrMu.Unlock()

	entry, err := jrLatestActive(mc)

	if err != nil {
		return nil, err
	}

	if guard != 0 {
		guarded, err := jrGetEntry(mc, guard)

		if err != nil {
			return nil, err
		}

		if guarded == nil {
			return nil, NotFound("GET /machine/v1/journal lists this user's machine-plane writes", "no journal entry %s for the bound user", idString(guard))
		}

		if guarded.Undone {
			ops, _ := jrDecodeOps(guarded)
			mc.SetMeta("replayed", true)

			return map[string]any{"undone": false, "alreadyUndone": true, "entry": jrEntryView(guarded, ops), "note": jrUndoNote}, nil
		}

		if entry == nil || entry.JournalId != guard {
			return nil, Conflict("undo reverses the most recent write first; undo the newer entries (GET /machine/v1/journal) or omit journal_id", "journal entry %s is not the most recent un-undone write", idString(guard)).WithDetails(map[string]any{"next": jrNextId(entry)})
		}
	}

	if entry == nil {
		return nil, NotFound("GET /machine/v1/journal shows what has been undone; "+jrUndoNote, "there is no machine-plane write left to undo for the bound user")
	}

	ops, err := jrDecodeOps(entry)

	if err != nil {
		return nil, err
	}

	reversed, err := jrRunUndoOps(mc, ops)

	if err != nil {
		auditWrite(mc, reversed, false)
		return nil, jrWithDetails(toFail(err), map[string]any{"journalId": idString(entry.JournalId), "route": entry.Route})
	}

	if err := jrMarkUndone(mc, entry.JournalId, true); err != nil {
		return nil, err
	}

	entry.Undone = true
	entry.UndoneUnixTime = time.Now().Unix()
	auditWrite(mc, entry.Changes, true)

	next, _ := jrLatestActive(mc)

	return map[string]any{
		"undone":        true,
		"entry":         jrEntryView(entry, ops),
		"stepsReversed": reversed,
		"redoAvailable": len(ops) > 0 && jrPreflightRedo(ops) == nil,
		"nextUndo":      jrNextId(next),
		"note":          jrUndoNote,
	}, nil
}

func jrNextId(entry *MachineJournal) any {
	if entry == nil {
		return nil
	}

	return idString(entry.JournalId)
}

func jrHandleRedo(mc *Ctx) (any, error) {
	var req jrUndoRequest

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if err := mc.RequireWriteTier(); err != nil {
		return nil, err
	}

	var guard int64

	if strings.TrimSpace(req.JournalId) != "" {
		id, err := ResolveId("journal_id", req.JournalId)

		if err != nil {
			return nil, err
		}

		guard = id
	}

	jrMu.Lock()
	defer jrMu.Unlock()

	entry, err := jrRedoCandidate(mc)

	if err != nil {
		return nil, err
	}

	if guard != 0 {
		guarded, err := jrGetEntry(mc, guard)

		if err != nil {
			return nil, err
		}

		if guarded == nil {
			return nil, NotFound("GET /machine/v1/journal lists this user's machine-plane writes", "no journal entry %s for the bound user", idString(guard))
		}

		if !guarded.Undone {
			ops, _ := jrDecodeOps(guarded)
			mc.SetMeta("replayed", true)

			return map[string]any{"redone": false, "alreadyApplied": true, "entry": jrEntryView(guarded, ops)}, nil
		}

		if entry == nil || entry.JournalId != guard {
			return nil, Conflict("redo re-applies the most recently undone write first, and only until a newer write lands; omit journal_id to redo the next one", "journal entry %s is not next in line for redo", idString(guard)).WithDetails(map[string]any{"next": jrNextId(entry)})
		}
	}

	if entry == nil {
		return nil, NotFound("redo is available only right after an undo, until a newer write lands; GET /machine/v1/journal shows the history", "there is no undone machine-plane write to redo for the bound user")
	}

	ops, err := jrDecodeOps(entry)

	if err != nil {
		return nil, err
	}

	redone, err := jrRunRedoOps(mc, ops)

	if err != nil {
		auditWrite(mc, redone, false)
		return nil, jrWithDetails(toFail(err), map[string]any{"journalId": idString(entry.JournalId), "route": entry.Route})
	}

	if err := jrMarkUndone(mc, entry.JournalId, false); err != nil {
		return nil, err
	}

	entry.Undone = false
	entry.UndoneUnixTime = 0
	auditWrite(mc, entry.Changes, true)

	return map[string]any{
		"redone":      true,
		"entry":       jrEntryView(entry, ops),
		"stepsRedone": redone,
	}, nil
}

// ---------------------------------------------------------------------------------------------
// GET /journal
// ---------------------------------------------------------------------------------------------

// jrClampLimit applies the list-route limit rule: default, clamp to the cap (never reject)
func jrClampLimit(requested int64, def, max int) (int, bool) {
	if requested <= 0 {
		return def, false
	}

	if requested > int64(max) {
		return max, true
	}

	return int(requested), false
}

func jrHandleJournal(mc *Ctx) (any, error) {
	rawLimit, err := mc.QueryInt("limit", 0)

	if err != nil {
		return nil, err
	}

	offset, err := mc.QueryInt("offset", 0)

	if err != nil {
		return nil, err
	}

	if offset < 0 {
		return nil, Invalid("offset is 0 or more", "offset %d is negative", offset)
	}

	includeUndone, err := mc.QueryBool("include_undone", true)

	if err != nil {
		return nil, err
	}

	limit, clamped := jrClampLimit(rawLimit, DefaultLimit, MaxLimit)
	db, err := jrDB(mc)

	if err != nil {
		return nil, err
	}

	sess := db.NewSession(mc.Web).Where("uid=?", mc.Uid)

	if !includeUndone {
		sess = sess.And("undone=?", false)
	}

	var rows []*MachineJournal

	if err := sess.OrderBy("journal_id desc").Limit(limit+1, int(offset)).Find(&rows); err != nil {
		return nil, err
	}

	more := len(rows) > limit

	if more {
		rows = rows[:limit]
	}

	entries := make([]map[string]any, 0, len(rows))

	for _, row := range rows {
		ops, derr := jrDecodeOps(row)

		if derr != nil {
			errfile.Expected("listing a journal entry whose inverse could not be decoded", derr)
			ops = nil
		}

		entries = append(entries, jrEntryView(row, ops))
	}

	if clamped {
		mc.Truncated(limit)
	} else if more {
		mc.SetMeta("truncated", true)
		mc.SetMeta("limitApplied", limit)
	}

	nextUndo, err := jrLatestActive(mc)

	if err != nil {
		return nil, err
	}

	redo, err := jrRedoCandidate(mc)

	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"entries":  entries,
		"nextUndo": jrNextId(nextUndo),
		"nextRedo": jrNextId(redo),
		"note":     jrUndoNote,
	}

	if more {
		out["nextOffset"] = int(offset) + limit
	}

	return out, nil
}

// ---------------------------------------------------------------------------------------------
// POST /batch · GET /batch/ops
// ---------------------------------------------------------------------------------------------

// jrBatchExcludedPrefixes are write routes a batch never carries: the journal routes themselves,
// ingest (which has its own plan/apply protocol and streams) and the passthrough
var jrBatchExcludedPrefixes = []string{"/batch", "/undo", "/redo", "/ingest", "/api/", "/admin"}

// jrBatchOps returns the closed op set: every live, dry-runnable write-tier route not excluded
func jrBatchOps() []*RouteDef {
	var out []*RouteDef
	all := Routes()

	for i := range all {
		r := &all[i]

		if !jrIsBatchable(r) {
			continue
		}

		out = append(out, r)
	}

	return out
}

func jrIsBatchable(r *RouteDef) bool {
	if r.Tier != TierWrite || !r.DryRunnable || r.Status != StatusLive || r.NoUser {
		return false
	}

	for _, p := range jrBatchExcludedPrefixes {
		if r.Path == strings.TrimSuffix(p, "/") || strings.HasPrefix(r.Path, p) {
			return false
		}
	}

	return true
}

func jrOpName(r *RouteDef) string {
	return r.Method + " " + r.Path
}

func jrHandleBatchOps(mc *Ctx) (any, error) {
	ops := jrBatchOps()
	out := make([]map[string]any, 0, len(ops))

	for _, r := range ops {
		out = append(out, map[string]any{"op": jrOpName(r), "method": r.Method, "path": r.Path, "summary": r.Summary, "params": jrPathParamNames(r.Path)})
	}

	return map[string]any{"ops": out, "maxOperations": jrMaxBatchOps, "maxChangesDefault": DefaultMaxChanges}, nil
}

func jrPathParamNames(path string) []string {
	names := []string{}

	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, ":") || strings.HasPrefix(seg, "*") {
			names = append(names, seg[1:])
		}
	}

	return names
}

// jrMatchPath matches a concrete or templated path against a route template and returns the
// params. A templated segment in the path (":id") is filled from params.
func jrMatchPath(template, path string, params map[string]string) (gin.Params, bool) {
	tSegs := strings.Split(strings.Trim(template, "/"), "/")
	pSegs := strings.Split(strings.Trim(path, "/"), "/")
	var out gin.Params

	for i, t := range tSegs {
		if strings.HasPrefix(t, "*") {
			rest := ""

			if i < len(pSegs) {
				rest = "/" + strings.Join(pSegs[i:], "/")
			}

			out = append(out, gin.Param{Key: t[1:], Value: rest})

			return out, true
		}

		if i >= len(pSegs) {
			return nil, false
		}

		p := pSegs[i]

		if strings.HasPrefix(t, ":") {
			name := t[1:]

			if p == t {
				v, ok := params[name]

				if !ok || strings.TrimSpace(v) == "" {
					return nil, false
				}

				p = v
			} else if strings.HasPrefix(p, ":") {
				return nil, false
			}

			if p == "" {
				return nil, false
			}

			out = append(out, gin.Param{Key: name, Value: p})
			continue
		}

		if t != p {
			return nil, false
		}
	}

	if len(pSegs) != len(tSegs) {
		return nil, false
	}

	return out, true
}

// jrConcretePath renders a template with its params, for the preview and the sub-request URL
func jrConcretePath(template string, params gin.Params) string {
	segs := strings.Split(template, "/")

	for i, s := range segs {
		if strings.HasPrefix(s, ":") || strings.HasPrefix(s, "*") {
			if v, ok := params.Get(s[1:]); ok {
				segs[i] = strings.TrimPrefix(v, "/")
			}
		}
	}

	return strings.Join(segs, "/")
}

type jrBatchOperation struct {
	Op     string            `json:"op"`
	Params map[string]string `json:"params,omitempty"`
	Query  map[string]string `json:"query,omitempty"`
	Args   json.RawMessage   `json:"args,omitempty"`
}

type jrBatchRequest struct {
	WriteOpts
	Operations []jrBatchOperation `json:"operations"`
	Summary    string             `json:"summary,omitempty"`
}

// jrReservedArgs are the write-protocol arguments the batch sets on each operation itself
var jrReservedArgs = []string{"dry_run", "confirm_token", "max_changes"}

// jrResolvedOp is one validated batch operation
type jrResolvedOp struct {
	index  int
	route  *RouteDef
	params gin.Params
	path   string
	query  url.Values
	args   map[string]json.RawMessage
}

// jrNonChangeKinds are change-count keys that do not count against the ceiling
var jrNonChangeKinds = map[string]bool{
	"unchanged": true, "skipped": true, "skip": true, "duplicate": true, "duplicates": true,
	"ignored": true, "blocked": true, "ambiguous": true, "noop": true, "no_change": true,
}

// jrCountChanges sums a write result's change counts, leaving out the kinds that change nothing
func jrCountChanges(changes map[string]int) int {
	n := 0

	for k, v := range changes {
		if jrNonChangeKinds[strings.ToLower(k)] || v < 0 {
			continue
		}

		n += v
	}

	return n
}

// jrValidateBatch turns the request into resolved operations without calling any handler
func jrValidateBatch(req *jrBatchRequest) ([]*jrResolvedOp, error) {
	if len(req.Operations) == 0 {
		return nil, Invalid("pass operations: [{op: \"POST /transactions\", args: {...}}]; GET /machine/v1/batch/ops lists the ops", "a batch needs at least one operation")
	}

	if len(req.Operations) > jrMaxBatchOps {
		return nil, Invalid(fmt.Sprintf("split the batch into batches of %d operations or fewer", jrMaxBatchOps), "%d operations exceed the batch limit of %d", len(req.Operations), jrMaxBatchOps)
	}

	ops := jrBatchOps()
	resolved := make([]*jrResolvedOp, 0, len(req.Operations))

	for i, o := range req.Operations {
		method, path, ok := strings.Cut(strings.TrimSpace(o.Op), " ")
		method = strings.ToUpper(strings.TrimSpace(method))
		path = strings.TrimSpace(path)

		if !ok || method == "" || !strings.HasPrefix(path, "/") {
			return nil, Invalid("op is \"METHOD /path\", e.g. \"PATCH /transactions/:id\" with params {\"id\": \"…\"}", "operations[%d].op %q is not \"METHOD /path\"", i, o.Op).WithDetails(map[string]any{"index": i})
		}

		if strings.HasPrefix(path, BasePath+"/") {
			path = strings.TrimPrefix(path, BasePath)
		}

		var match *RouteDef
		var params gin.Params

		for _, r := range ops {
			if r.Method != method {
				continue
			}

			if p, ok := jrMatchPath(r.Path, path, o.Params); ok {
				match, params = r, p
				break
			}
		}

		if match == nil {
			return nil, Invalid("GET /machine/v1/batch/ops lists the operations a batch accepts; path parameters go in params", "operations[%d].op %q is not a batchable write operation", i, o.Op).WithDetails(map[string]any{"index": i})
		}

		args := map[string]json.RawMessage{}

		if len(bytes.TrimSpace(o.Args)) > 0 && string(bytes.TrimSpace(o.Args)) != "null" {
			if err := json.Unmarshal(o.Args, &args); err != nil {
				errfile.Expected("parsing a batch operation's args", err)
				return nil, Invalid("args is a JSON object of the operation's own arguments", "operations[%d].args is not a JSON object", i).WithDetails(map[string]any{"index": i})
			}
		}

		for _, k := range jrReservedArgs {
			if _, has := args[k]; has {
				return nil, Invalid("remove "+k+" from the operation; the batch's own dry_run, confirm_token and max_changes govern every operation", "operations[%d].args carries %s", i, k).WithDetails(map[string]any{"index": i})
			}
		}

		query := url.Values{}

		for k, v := range o.Query {
			query.Set(k, v)
		}

		resolved = append(resolved, &jrResolvedOp{index: i, route: match, params: params, path: jrConcretePath(match.Path, params), query: query, args: args})
	}

	return resolved, nil
}

// jrSubCtx builds the context one batch operation runs in: its own route (so its confirm token is
// bound to it), its own path params, query and body, the batch's user, zone, client and tier
func jrSubCtx(mc *Ctx, op *jrResolvedOp, body []byte) *Ctx {
	target := BasePath + op.path

	if len(op.query) > 0 {
		target += "?" + op.query.Encode()
	}

	req := httptest.NewRequest(op.route.Method, target, bytes.NewReader(body))

	if mc.Gin != nil && mc.Gin.Request != nil {
		req = req.WithContext(mc.Gin.Request.Context())
		req.RemoteAddr = mc.Gin.Request.RemoteAddr
		req.Host = mc.Gin.Request.Host

		for _, h := range []string{core.ClientTimezoneNameHeaderName, core.ClientTimezoneOffsetHeaderName, HeaderClient, core.AcceptLanguageHeaderName} {
			if v := mc.Gin.Request.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
	}

	req.Header.Set("Content-Type", "application/json")

	engine := upstreamEngine

	if engine == nil {
		engine = gin.New()
	}

	gc := gin.CreateTestContextOnly(httptest.NewRecorder(), engine)
	gc.Request = req
	gc.Params = op.params

	web := core.WrapWebContext(gc, mc.Config.TrustedProxyIPs)

	if mc.Web != nil {
		web.SetContextId(mc.Web.GetContextId())
	}

	return &Ctx{
		Gin:         gc,
		Web:         web,
		Config:      mc.Config,
		Route:       op.route,
		Uid:         mc.Uid,
		User:        mc.User,
		Loc:         mc.Loc,
		Client:      mc.Client,
		WriteTierOn: mc.WriteTierOn,
		body:        body,
		bodyRead:    true,
	}
}

// jrCallOp runs one operation's handler with the given write-protocol settings
func jrCallOp(mc *Ctx, op *jrResolvedOp, dryRun bool, confirmToken string, maxChanges int) (res *WriteResult, sub *Ctx, err error) {
	args := make(map[string]json.RawMessage, len(op.args)+3)

	for k, v := range op.args {
		args[k] = v
	}

	args["dry_run"] = json.RawMessage(strconv.FormatBool(dryRun))
	args["max_changes"] = json.RawMessage(strconv.Itoa(maxChanges))

	if confirmToken != "" {
		tok, _ := json.Marshal(confirmToken)
		args["confirm_token"] = tok
	}

	body, err := json.Marshal(args)

	if err != nil {
		return nil, nil, err
	}

	sub = jrSubCtx(mc, op, body)

	defer func() {
		if rec := recover(); rec != nil {
			log.Errorf(mc.Web, "[machine.batch] panic in operation %d (%s): %v", op.index, jrOpName(op.route), rec)
			err = NewFail(CodeInternal, "read ~/T/ezbookkeeping/error.err for the server-side detail", "operation %d failed", op.index)
		}
	}()

	result, herr := op.route.Handler(sub)

	if herr != nil {
		return nil, sub, herr
	}

	wr, ok := result.(*WriteResult)

	if !ok {
		return nil, sub, NewFail(CodeInternal, "run this operation through its own route instead of /batch", "operation %d (%s) did not answer with the write protocol", op.index, jrOpName(op.route))
	}

	return wr, sub, nil
}

// jrOpPlan is the resolve-time view of one operation
type jrOpPlan struct {
	Index       int            `json:"index"`
	Op          string         `json:"op"`
	Path        string         `json:"path"`
	Changes     map[string]int `json:"changes"`
	Count       int            `json:"count"`
	Fingerprint string         `json:"fingerprint"`
	Preview     any            `json:"preview,omitempty"`
	Warnings    []string       `json:"warnings,omitempty"`
}

func jrHandleBatch(mc *Ctx) (any, error) {
	var req jrBatchRequest

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	resolvedOps, err := jrValidateBatch(&req)

	if err != nil {
		return nil, err
	}

	maxChanges := req.MaxChanges

	if maxChanges <= 0 {
		maxChanges = DefaultMaxChanges
	}

	resolve := func() (*Plan, error) {
		plans := make([]jrOpPlan, 0, len(resolvedOps))
		totals := map[string]int{}
		count := 0
		var warnings []string

		for _, op := range resolvedOps {
			wr, _, err := jrCallOp(mc, op, true, "", maxChanges)

			if err != nil {
				f := toFail(err)

				return nil, jrWithDetails(f, map[string]any{"failedIndex": op.index, "op": jrOpName(op.route), "path": op.path, "phase": "plan"})
			}

			n := jrCountChanges(wr.Changes)
			count += n

			for k, v := range wr.Changes {
				totals[k] += v
			}

			for _, w := range wr.Warnings {
				warnings = append(warnings, fmt.Sprintf("operations[%d]: %s", op.index, w))
			}

			plans = append(plans, jrOpPlan{Index: op.index, Op: jrOpName(op.route), Path: op.path, Changes: wr.Changes, Count: n, Fingerprint: wr.Fingerprint, Preview: wr.Preview, Warnings: wr.Warnings})
		}

		warnings = append(warnings, "operations are planned against the books as they stand now; an operation cannot refer to something an earlier operation in the same batch creates")

		fp := make([]map[string]any, 0, len(plans))

		for _, p := range plans {
			fp = append(fp, map[string]any{"op": p.Op, "path": p.Path, "fingerprint": p.Fingerprint, "changes": p.Changes})
		}

		return &Plan{
			Changes:       totals,
			Count:         count,
			Preview:       map[string]any{"operations": plans, "operationCount": len(plans)},
			Fingerprinted: fp,
			Warnings:      warnings,
			State:         plans,
		}, nil
	}

	apply := func(p *Plan) (any, error) {
		plans := p.State.([]jrOpPlan)

		jrMu.Lock()
		defer jrMu.Unlock()

		var journalIds []int64
		var applied []int
		results := make([]map[string]any, 0, len(resolvedOps))
		var notUndoable []int

		fail := func(idx int, op *jrResolvedOp, cause error) error {
			rolledBack, rollbackErrs := jrRollbackBatch(mc, journalIds, applied, notUndoable)
			f := toFail(cause)
			details := map[string]any{
				"failedIndex": idx,
				"op":          jrOpName(op.route),
				"path":        op.path,
				"applied":     len(applied),
				"rolledBack":  rolledBack,
			}

			if len(rollbackErrs) > 0 {
				details["rollbackErrors"] = rollbackErrs
				f = jrWithDetails(f, details)
				f.Hint = f.Hint + "; the rollback was incomplete — GET /machine/v1/journal and check the operations named in rollbackErrors"

				return f
			}

			return jrWithDetails(f, details)
		}

		for i, op := range resolvedOps {
			dry, _, err := jrCallOp(mc, op, true, "", maxChanges)

			if err != nil {
				return nil, fail(op.index, op, err)
			}

			if dry.Fingerprint != plans[i].Fingerprint {
				return nil, fail(op.index, op, Conflict("an earlier operation in the batch, or another writer, changed what this operation would do; re-plan the batch", "operations[%d] no longer matches its preview", op.index).WithDetails(map[string]any{"changes": dry.Changes}))
			}

			done, sub, err := jrCallOp(mc, op, false, dry.ConfirmToken, maxChanges)

			if err != nil {
				return nil, fail(op.index, op, err)
			}

			applied = append(applied, op.index)
			jid := done.JournalId

			if jid == 0 && sub != nil {
				if v, ok := sub.meta["journalId"].(int64); ok {
					jid = v
				}
			}

			if jid != 0 {
				journalIds = append(journalIds, jid)
			} else {
				notUndoable = append(notUndoable, op.index)
			}

			results = append(results, map[string]any{"index": op.index, "op": jrOpName(op.route), "path": op.path, "changes": done.Changes, "result": done.Result})
		}

		out := map[string]any{"operations": results, "applied": len(applied), "rolledBack": 0}

		if len(notUndoable) > 0 {
			out["notUndoable"] = notUndoable
		}

		if err := jrMergeJournal(mc, req.Summary, resolvedOps, journalIds, p.Count); err != nil {
			log.Warnf(mc.Web, "[machine.batch] merging %d journal entries failed: %s", len(journalIds), err.Error())
			out["journalWarning"] = "the operations were applied, but they are journaled as separate entries; undo them one at a time"
			out["journalIds"] = jrIdStrings(journalIds)
		}

		return out, nil
	}

	return RunWrite(mc, req.WriteOpts, resolve, apply)
}

func jrIdStrings(ids []int64) []string {
	out := make([]string, 0, len(ids))

	for _, id := range ids {
		out = append(out, idString(id))
	}

	return out
}

// jrRollbackBatch reverses the applied operations through their journal entries, newest first
func jrRollbackBatch(mc *Ctx, journalIds []int64, applied []int, notUndoable []int) (int, []string) {
	var errsOut []string
	rolledBack := 0

	for i := len(journalIds) - 1; i >= 0; i-- {
		entry, err := jrGetEntry(mc, journalIds[i])

		if err != nil || entry == nil {
			errfile.Caught("reading a journal entry while rolling back a batch", err, errfile.F("journal_id", journalIds[i]))
			errsOut = append(errsOut, fmt.Sprintf("journal entry %d could not be read", journalIds[i]))
			continue
		}

		ops, err := jrDecodeOps(entry)

		if err != nil {
			errfile.Expected("decoding a journal entry while rolling back a batch", err)
			errsOut = append(errsOut, fmt.Sprintf("journal entry %d is unreadable", journalIds[i]))
			continue
		}

		if _, err := jrRunUndoOps(mc, ops); err != nil {
			errsOut = append(errsOut, fmt.Sprintf("journal entry %d (%s): %s", journalIds[i], entry.Route, toFail(err).Message))
			continue
		}

		if err := jrDeleteEntries(mc, []int64{entry.JournalId}); err != nil {
			errfile.Caught("deleting a journal entry while rolling back a batch", err, errfile.F("journal_id", entry.JournalId))
			_ = jrMarkUndone(mc, entry.JournalId, true)
		}

		rolledBack++
	}

	for _, idx := range notUndoable {
		errsOut = append(errsOut, fmt.Sprintf("operations[%d] recorded no journal entry and could not be reversed", idx))
	}

	return rolledBack, errsOut
}

// jrMergeJournal folds the operations' journal entries into ONE entry for the batch, so one /undo
// reverses the whole batch; the per-operation entries are then removed
func jrMergeJournal(mc *Ctx, summary string, ops []*jrResolvedOp, journalIds []int64, changes int) error {
	if len(journalIds) == 0 {
		return nil
	}

	var all []InverseOp

	for _, id := range journalIds {
		entry, err := jrGetEntry(mc, id)

		if err != nil {
			return err
		}

		if entry == nil {
			return fmt.Errorf("journal entry %d vanished", id)
		}

		entryOps, err := jrDecodeOps(entry)

		if err != nil {
			return err
		}

		all = append(all, entryOps...)
	}

	if strings.TrimSpace(summary) == "" {
		summary = jrBatchSummary(ops)
	}

	if _, err := RecordJournal(mc, summary, changes, all); err != nil {
		return err
	}

	return jrDeleteEntries(mc, journalIds)
}

func jrBatchSummary(ops []*jrResolvedOp) string {
	counts := map[string]int{}
	var order []string

	for _, op := range ops {
		name := jrOpName(op.route)

		if counts[name] == 0 {
			order = append(order, name)
		}

		counts[name]++
	}

	parts := make([]string, 0, len(order))

	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%d× %s", counts[name], name))
	}

	return fmt.Sprintf("batch of %d operations: %s", len(ops), strings.Join(parts, ", "))
}
