package machine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/errs"
	"github.com/mayswind/ezbookkeeping/pkg/log"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
	"github.com/mayswind/ezbookkeeping/pkg/utils"
)

// ingest_apply.go — the write half (apis.mdx §14.8). The apply recomputes the whole plan through
// ingEvaluate, refuses if the fingerprint moved (RunWrite), then submits the new rows through the
// SAME service path upstream's TransactionImportHandler uses (TransactionService.
// BatchCreateTransactions), in batches, reporting progress; writes machine_import_record and one
// journal entry whose inverse soft-deletes exactly the rows it created (ing.import); and returns the
// created counts per account.
//
// The description goes into the new transaction's comment ONLY on create. A re-run never touches
// an existing row, so a note the operator typed after import survives byte for byte.

const (
	ingBatchSize      = 500
	ingInverseImport  = "ing.import"
	ingInverseAccount = "ing.account_create"
	ingInverseMap     = "ing.map_restore"
	ingRunLog         = "_run.log"
)

// ingApplyMu serialises ingest applies (one writer, apis.mdx §18)
var ingApplyMu sync.Mutex

func init() {
	RegisterInverse(ingInverseImport, ingUndoImport)
}

// ingProgress streams NDJSON progress lines when the caller asked for them
// (Accept: application/x-ndjson); the final line is the normal envelope, written by the router
type ingProgress struct {
	mc      *Ctx
	on      bool
	started bool
	last    time.Time
}

func ingNewProgress(mc *Ctx) *ingProgress {
	return &ingProgress{mc: mc, on: strings.Contains(mc.Gin.GetHeader("Accept"), "application/x-ndjson")}
}

// Emit writes one progress line (throttled to 4 per second, except stage ends)
func (p *ingProgress) Emit(stage string, done, total int, extra map[string]any) {
	if p == nil || !p.on {
		return
	}

	now := time.Now()

	if p.started && done != total && done != 0 && now.Sub(p.last) < 250*time.Millisecond {
		return
	}

	p.last = now
	w := p.mc.Gin.Writer

	if !p.started {
		p.started = true
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	}

	line := map[string]any{"type": "progress", "stage": stage, "done": done, "total": total}

	if total > 0 {
		line["percent"] = done * 100 / total
	}

	for k, v := range extra {
		line[k] = v
	}

	data, _ := json.Marshal(line)
	_, _ = w.Write(append(data, '\n'))
	w.Flush()
}

// Func adapts Emit to the engine's progress callback
func (p *ingProgress) Func() func(string, int, int) {
	if p == nil || !p.on {
		return nil
	}

	return func(stage string, done, total int) { p.Emit(stage, done, total, nil) }
}

// ingNewRunId mints a run id: time-ordered, unique
func ingNewRunId() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)

	return "run_" + time.Now().UTC().Format("20060102T150405Z") + "_" + hex.EncodeToString(b)
}

// ingApplyResultAccount is one account's outcome
type ingApplyResultAccount struct {
	AccountKey string `json:"account_key"`
	AccountId  string `json:"account_id,omitempty"`
	Created    int    `json:"created"`
	Linked     int    `json:"linked"`
	Reimported int    `json:"reimported"`
	Blocked    bool   `json:"blocked"`
	Status     string `json:"status"` // ok | blocked | nothing_new | failed
}

// ingApplyResult is what an apply returns (inside WriteResult.result)
type ingApplyResult struct {
	RunId      string                   `json:"run_id"`
	Created    int                      `json:"created"`
	Linked     int                      `json:"linked"`
	Reimported int                      `json:"reimported"`
	Accounts   []*ingApplyResultAccount `json:"accounts"`
	AllOk      bool                     `json:"all_accounts_ok"`
	Report     string                   `json:"report"`
	Warnings   []string                 `json:"warnings"`
}

// ingRunReport is the full report of one run (GET /ingest/runs/:id)
type ingRunReport struct {
	RunId               string                   `json:"run_id"`
	Kind                string                   `json:"kind"`
	Route               string                   `json:"route"`
	User                string                   `json:"user"`
	Client              string                   `json:"client,omitempty"`
	Root                string                   `json:"root,omitempty"`
	Staging             string                   `json:"staging,omitempty"`
	Mode                string                   `json:"mode,omitempty"`
	File                string                   `json:"file,omitempty"`
	StartedAt           string                   `json:"started_at"`
	FinishedAt          string                   `json:"finished_at"`
	Outcome             string                   `json:"outcome"`
	Error               string                   `json:"error,omitempty"`
	Created             int                      `json:"created"`
	Linked              int                      `json:"linked"`
	Reimported          int                      `json:"reimported"`
	JournalId           int64                    `json:"journal_id,omitempty"`
	Fingerprint         string                   `json:"fingerprint,omitempty"`
	Range               map[string]any           `json:"range,omitempty"`
	Totals              map[string]int           `json:"totals"`
	Accounts            []*ingApplyResultAccount `json:"accounts"`
	Plan                []*ingPlanAccount        `json:"plan_accounts"`
	Unmapped            []*ingUnmapped           `json:"unmapped"`
	CategoryMap         map[string]string        `json:"category_map"`
	FallbackCategoryIds map[string]string        `json:"fallback_category_ids"`
	Fallback            []map[string]string      `json:"fallback_transactions"`
	Warnings            []string                 `json:"warnings"`
}

// ingRunsDir is where run reports live: the plane's state directory, per user (never the repo)
func ingRunsDir(uid int64) string {
	return filepath.Join(StateDir(), "ingest-runs", strconv.FormatInt(uid, 10))
}

func ingSaveRunReport(uid int64, rep *ingRunReport) {
	defer func() { _ = recover() }()

	dir := ingRunsDir(uid)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}

	data, _ := json.MarshalIndent(rep, "", "  ")
	_ = ingWriteFileAtomic(dir, filepath.Join(dir, rep.RunId+".json"), append(data, '\n'))
}

// ingLoadRunReport reads one run report
func ingLoadRunReport(uid int64, runId string) (*ingRunReport, error) {
	if !ingRunIdPattern(runId) {
		return nil, Invalid("run ids look like run_20260921T184102Z_1a2b3c4d (GET /machine/v1/ingest/runs)", "%q is not a run id", runId)
	}

	data, err := os.ReadFile(filepath.Join(ingRunsDir(uid), runId+".json"))

	if errors.Is(err, fs.ErrNotExist) {
		return nil, NotFound("GET /machine/v1/ingest/runs lists the runs", "no run %s", runId)
	}

	if err != nil {
		return nil, NewFail(CodeInternal, "check the state directory's permissions", "cannot read the run report")
	}

	rep := &ingRunReport{}

	if err := json.Unmarshal(data, rep); err != nil {
		return nil, NewFail(CodeInternal, "the run report is damaged", "cannot parse the run report")
	}

	return rep, nil
}

func ingRunIdPattern(s string) bool {
	if !strings.HasPrefix(s, "run_") || len(s) > 64 {
		return false
	}

	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}

	return true
}

// IngestRunReport returns a run's report as generic JSON, for other families (the analytics
// import-fallout route reads fallback_transactions from it)
func IngestRunReport(uid int64, runId string) (map[string]any, error) {
	rep, err := ingLoadRunReport(uid, runId)

	if err != nil {
		return nil, err
	}

	data, _ := json.Marshal(rep)
	out := map[string]any{}
	_ = json.Unmarshal(data, &out)

	return out, nil
}

// ingPlanFingerprintSource is what the confirm token covers
func ingPlanFingerprintSource(p *ingPlanResult) any {
	return p.changes
}

// ingPlanToWritePlan wraps an ingest plan for RunWrite
func ingPlanToWritePlan(p *ingPlanResult) *Plan {
	return &Plan{
		Changes: map[string]int{
			"create":           len(p.changes.Creates),
			"link":             len(p.changes.Links),
			"reimport_deleted": p.Totals["reimport_deleted"],
			"already_present":  p.Totals["already_present"],
			"blocked":          p.Totals["blocked_rows"],
			"possible_matches": p.Totals["possible_matches"],
		},
		Count:         p.changes.Count(),
		Preview:       p,
		Fingerprinted: ingPlanFingerprintSource(p),
		Warnings:      p.Warnings,
		State:         p,
	}
}

// ingApplyChanges performs a plan's change set. kind is "statements" or "file".
func ingApplyChanges(mc *Ctx, plan *ingPlanResult, fingerprint string, prog *ingProgress) (*ingApplyResult, error) {
	ingApplyMu.Lock()
	defer ingApplyMu.Unlock()

	if err := ingRequireImportAllowed(mc); err != nil {
		return nil, err
	}

	started := time.Now()
	runId := ingNewRunId()
	cs := plan.changes
	res := &ingApplyResult{RunId: runId, Accounts: []*ingApplyResultAccount{}, Warnings: []string{}, Report: "GET /machine/v1/ingest/runs/" + runId}
	perAccount := map[string]*ingApplyResultAccount{}

	for _, pa := range plan.Accounts {
		ra := &ingApplyResultAccount{AccountKey: pa.AccountKey, AccountId: pa.AccountId, Blocked: pa.Blocked, Status: "nothing_new"}

		if pa.Blocked {
			ra.Status = "blocked"
		}

		perAccount[pa.AccountKey] = ra
		res.Accounts = append(res.Accounts, ra)
	}

	report := &ingRunReport{RunId: runId, Kind: plan.Kind, Route: mc.Route.Method + " " + mc.Route.Path, Client: mc.Client, Root: plan.Root, Staging: plan.Staging, Mode: plan.Mode,
		StartedAt: started.UTC().Format(time.RFC3339), Fingerprint: fingerprint, Range: plan.Range, Totals: plan.Totals, Accounts: res.Accounts, Plan: plan.Accounts, Unmapped: plan.Unmapped,
		CategoryMap: plan.CategoryMap, FallbackCategoryIds: plan.FallbackCategoryIds, Fallback: []map[string]string{}, Warnings: plan.Warnings}

	if mc.User != nil {
		report.User = mc.User.Username
	}

	if plan.Kind == "file" && len(plan.Accounts) > 0 && plan.Accounts[0].input != nil && len(plan.Accounts[0].input.Statements) > 0 {
		report.File = plan.Accounts[0].input.Statements[0].File
	}

	// the edit scope is upstream's rule for every created row
	if mc.User != nil {
		accounts, err := ingLoadAccounts(mc)

		if err != nil {
			return nil, err
		}

		refused := 0

		for _, c := range cs.Creates {
			aid, _ := strconv.ParseInt(c.AccountId, 10, 64)
			did, _ := strconv.ParseInt(c.DestAccountId, 10, 64)

			if !mc.User.CanEditTransactionByTransactionTime(utils.GetMinTransactionTimeFromUnixTime(c.Time), mc.Loc, accounts.ById[aid], accounts.ById[did]) {
				refused++
			}
		}

		if refused > 0 {
			return nil, NewFail(CodeForbidden, "widen the bound user's transaction edit scope in the browser (Settings), or narrow the import with start", "%d row(s) fall outside the bound user's transaction edit scope", refused)
		}
	}

	var created []*models.Transaction
	var createdOf []*ingCreate
	var failure error

	prog.Emit("import", 0, len(cs.Creates), map[string]any{"run_id": runId})

	for start := 0; start < len(cs.Creates); start += ingBatchSize {
		end := start + ingBatchSize

		if end > len(cs.Creates) {
			end = len(cs.Creates)
		}

		batch := cs.Creates[start:end]
		txns := make([]*models.Transaction, 0, len(batch))

		for _, c := range batch {
			t, err := ingTransactionModel(mc, c)

			if err != nil {
				failure = err
				break
			}

			txns = append(txns, t)
		}

		if failure != nil {
			break
		}

		if err := services.Transactions.BatchCreateTransactions(mc.Web, mc.Uid, txns, nil, nil); err != nil {
			log.Errorf(mc.Web, "[machine.ingest] batch create failed for uid %d after %d created: %s", mc.Uid, len(created), err.Error())
			failure = ingServiceFail(err, "the server could not create the transactions")

			break
		}

		// records for this batch, right away: a created row without its record would re-import
		var writes []ingRecordWrite

		for i, c := range batch {
			relink := map[string]bool{}

			for _, id := range c.Relink {
				relink[id] = true
			}

			for j, importId := range c.ImportIds {
				key := c.AccountKey

				if j == 1 && c.DestAccountKey != "" {
					key = c.DestAccountKey
				}

				src := ""

				if j < len(c.SourceFiles) {
					src = c.SourceFiles[j]
				} else if len(c.SourceFiles) > 0 {
					src = c.SourceFiles[0]
				}

				writes = append(writes, ingRecordWrite{ImportId: importId, TransactionId: txns[i].TransactionId, AccountKey: key, SourceFile: src, Relink: relink[importId]})
			}
		}

		if err := ingWriteRecords(mc, runId, writes); err != nil {
			// never leave rows without records: take this batch back out
			for _, t := range txns {
				if derr := services.Transactions.DeleteTransaction(mc.Web, mc.Uid, t.TransactionId); derr != nil {
					log.Errorf(mc.Web, "[machine.ingest] could not remove unrecorded transaction %d: %s", t.TransactionId, derr.Error())
				}
			}

			failure = err

			break
		}

		created = append(created, txns...)
		createdOf = append(createdOf, batch...)

		for _, c := range batch {
			if ra := perAccount[c.AccountKey]; ra != nil {
				ra.Created++

				if len(c.Relink) > 0 {
					ra.Reimported++
				}
			}

			if c.DestAccountKey != "" {
				if ra := perAccount[c.DestAccountKey]; ra != nil {
					ra.Created++
				}
			}

			if len(c.Relink) > 0 {
				res.Reimported += len(c.Relink)
			}
		}

		prog.Emit("import", len(created), len(cs.Creates), map[string]any{"run_id": runId})
	}

	var linked []*ingLink

	if failure == nil && len(cs.Links) > 0 {
		var writes []ingRecordWrite

		for _, l := range cs.Links {
			id, _ := strconv.ParseInt(l.TransactionId, 10, 64)
			writes = append(writes, ingRecordWrite{ImportId: l.ImportId, TransactionId: id, AccountKey: l.AccountKey, SourceFile: l.SourceFile, Relink: l.Relink})
		}

		if err := ingWriteRecords(mc, runId, writes); err != nil {
			failure = err
		} else {
			linked = cs.Links

			for _, l := range cs.Links {
				if ra := perAccount[l.AccountKey]; ra != nil {
					ra.Linked++
				}
			}

			prog.Emit("link", len(cs.Links), len(cs.Links), nil)
		}
	}

	res.Created = len(created)
	res.Linked = len(linked)

	// the journal entry: its inverse soft-deletes exactly what this run created
	if len(created) > 0 || len(linked) > 0 {
		op, err := ingBuildImportInverse(mc, runId, created, createdOf, linked)

		if err != nil {
			res.Warnings = append(res.Warnings, "the undo journal entry could not be prepared; undo this run by deleting its transactions (GET /ingest/runs/"+runId+")")
		} else if jid, jerr := RecordJournal(mc, "ingest "+plan.Kind+" "+runId+": "+strconv.Itoa(len(created))+" created, "+strconv.Itoa(len(linked))+" linked", len(created)+len(linked), []InverseOp{op}); jerr != nil {
			res.Warnings = append(res.Warnings, "the undo journal entry could not be written; undo this run by deleting its transactions (GET /ingest/runs/"+runId+")")
		} else {
			report.JournalId = jid
		}
	}

	// which created rows went to a fallback category (for /analytics/import-fallout)
	viaFallback := map[string]bool{}

	for _, r := range plan.allRows {
		if r.CategoryVia == "fallback" {
			viaFallback[r.ImportId] = true
		}
	}

	for i, c := range createdOf {
		if viaFallback[c.ImportIds[0]] {
			report.Fallback = append(report.Fallback, map[string]string{"transaction_id": idString(created[i].TransactionId), "account_key": c.AccountKey, "category_id": c.CategoryId, "kind": c.Kind, "date": c.Date})
		}
	}

	allOk := failure == nil

	for _, ra := range res.Accounts {
		switch {
		case ra.Blocked:
			allOk = false
		case ra.Created > 0 || ra.Linked > 0:
			ra.Status = "ok"
		}
	}

	res.AllOk = allOk
	report.Created, report.Linked, report.Reimported = res.Created, res.Linked, res.Reimported
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	report.Outcome = "ok"

	if failure != nil {
		report.Outcome = "partial"

		if len(created) == 0 && len(linked) == 0 {
			report.Outcome = "failed"
		}

		report.Error = toFail(failure).Message
	} else if !allOk {
		report.Outcome = "ok_with_blocked_accounts"
	}

	ingSaveRunReport(mc.Uid, report)
	ingStagingRunLog(plan, report)

	if failure != nil {
		f := toFail(failure)
		details := map[string]any{"run_id": runId, "created_before_failure": len(created), "linked_before_failure": len(linked)}

		if report.JournalId > 0 {
			details["journal_id"] = report.JournalId
		}

		return nil, (&Fail{Code: f.Code, Message: f.Message, UpstreamCode: f.UpstreamCode,
			Hint: "the rows already created are recorded (a re-run skips them) and POST /undo reverses them; " + f.Hint}).WithDetails(details)
	}

	prog.Emit("done", 1, 1, map[string]any{"run_id": runId})

	return res, nil
}

func ingFindPlanAccount(plan *ingPlanResult, key string) *ingPlanAccount {
	for _, pa := range plan.Accounts {
		if pa.AccountKey == key {
			return pa
		}
	}

	return nil
}

// ingTransactionModel builds the upstream model exactly as upstream's createNewTransactionModel
// does for an import row
func ingTransactionModel(mc *Ctx, c *ingCreate) (*models.Transaction, error) {
	aid, err := ResolveId("account_id", c.AccountId)

	if err != nil {
		return nil, err
	}

	cid, err := ResolveId("category_id", c.CategoryId)

	if err != nil {
		return nil, err
	}

	t := &models.Transaction{
		Uid:               mc.Uid,
		CategoryId:        cid,
		TransactionTime:   utils.GetMinTransactionTimeFromUnixTime(c.Time),
		TimezoneUtcOffset: c.UtcOffset,
		AccountId:         aid,
		Amount:            c.Amount,
		Comment:           c.Comment,
		CreatedIp:         "127.0.0.1",
	}

	switch c.Kind {
	case "expense":
		t.Type = models.TRANSACTION_DB_TYPE_EXPENSE
	case "income":
		t.Type = models.TRANSACTION_DB_TYPE_INCOME
	case "transfer":
		did, err := ResolveId("dest_account_id", c.DestAccountId)

		if err != nil {
			return nil, err
		}

		t.Type = models.TRANSACTION_DB_TYPE_TRANSFER_OUT
		t.RelatedAccountId = did
		t.RelatedAccountAmount = c.DestAmount
	default:
		return nil, NewFail(CodeInternal, "this is a bug; report it", "unknown create kind %q", c.Kind)
	}

	return t, nil
}

// ingServiceFail maps a service error through the one upstream mapping table (envelope.go); an
// error that is not upstream's becomes upstream_error with the detail left in the log
func ingServiceFail(err error, message string) error {
	var ue *errs.Error

	if errors.As(err, &ue) {
		return Upstream(ue)
	}

	return NewFail(CodeUpstreamError, "read ~/T/ezbookkeeping/error.err for the server-side detail", "%s", message)
}

// ---- the inverse: ing.import ----------------------------------------------------------------

type ingImportInverse struct {
	RunId   string              `json:"run_id"`
	Created []ingInverseCreated `json:"created"`
	Linked  []ingInverseLinked  `json:"linked"`
}

type ingInverseCreated struct {
	TransactionId string   `json:"transaction_id"`
	ImportIds     []string `json:"import_ids"`
	// Prior maps a relinked import id to the (deleted) transaction its record pointed at before
	Prior map[string]string `json:"prior,omitempty"`
}

type ingInverseLinked struct {
	ImportId      string `json:"import_id"`
	TransactionId string `json:"transaction_id"`
	Prior         string `json:"prior_transaction_id,omitempty"`
}

type ingImportCheck struct {
	Transactions map[string]int64 `json:"transactions"` // id → updated_unix_time right after the import
}

func ingBuildImportInverse(mc *Ctx, runId string, created []*models.Transaction, createdOf []*ingCreate, linked []*ingLink) (InverseOp, error) {
	payload := ingImportInverse{RunId: runId, Created: []ingInverseCreated{}, Linked: []ingInverseLinked{}}
	check := ingImportCheck{Transactions: map[string]int64{}}
	ids := make([]int64, 0, len(created))

	for i, t := range created {
		ic := ingInverseCreated{TransactionId: idString(t.TransactionId), ImportIds: createdOf[i].ImportIds}

		for j, imp := range createdOf[i].Relink {
			if ic.Prior == nil {
				ic.Prior = map[string]string{}
			}

			if j < len(createdOf[i].PriorTxnIds) {
				ic.Prior[imp] = createdOf[i].PriorTxnIds[j]
			}
		}

		payload.Created = append(payload.Created, ic)
		ids = append(ids, t.TransactionId)
	}

	for _, l := range linked {
		payload.Linked = append(payload.Linked, ingInverseLinked{ImportId: l.ImportId, TransactionId: l.TransactionId, Prior: l.PriorTransaction})
	}

	states, err := ingLoadTxnStates(mc, ids)

	if err != nil {
		return InverseOp{}, err
	}

	for _, id := range ids {
		if st := states[id]; st != nil && st.Exists {
			check.Transactions[idString(id)] = st.UpdatedUnixTime
		}
	}

	return NewInverseOp(ingInverseImport, payload, check), nil
}

// ingUndoImport soft-deletes the transactions an import created — through the same service call
// the browser's delete makes — and removes (or re-points) the records it wrote. It refuses with a
// conflict naming the row when a created transaction was edited since the import.
func ingUndoImport(mc *Ctx, payload json.RawMessage, check json.RawMessage) error {
	var p ingImportInverse

	if err := json.Unmarshal(payload, &p); err != nil {
		return NewFail(CodeInternal, "the journal entry is damaged", "cannot read the import's inverse")
	}

	var c ingImportCheck
	_ = json.Unmarshal(check, &c)

	ids := make([]int64, 0, len(p.Created))

	for _, cr := range p.Created {
		id, err := ResolveId("transaction_id", cr.TransactionId)

		if err != nil {
			return err
		}

		ids = append(ids, id)
	}

	states, err := ingLoadTxnStates(mc, ids)

	if err != nil {
		return err
	}

	// preflight: every row still exactly as the import left it (or already deleted)
	var edited []string

	for _, id := range ids {
		st := states[id]

		if st == nil || !st.Exists || st.Deleted {
			continue
		}

		if want, ok := c.Transactions[idString(id)]; ok && st.UpdatedUnixTime != want {
			edited = append(edited, idString(id))
		}
	}

	if len(edited) > 0 {
		sort.Strings(edited)

		return Conflict("these transactions were edited after the import; delete them yourself if you still want the import gone", "%d imported transaction(s) changed since run %s", len(edited), p.RunId).WithDetails(map[string]any{"edited": edited, "run_id": p.RunId})
	}

	for _, id := range ids {
		st := states[id]

		if st == nil || !st.Exists || st.Deleted {
			continue
		}

		if err := services.Transactions.DeleteTransaction(mc.Web, mc.Uid, id); err != nil {
			log.Errorf(mc.Web, "[machine.ingest] undo could not delete transaction %d: %s", id, err.Error())
			return NewFail(CodeUpstreamError, "retry POST /undo; rows already removed stay removed", "could not delete transaction %s", idString(id))
		}
	}

	drop := map[string]int64{}
	restore := map[string][2]int64{}

	for i, cr := range p.Created {
		for _, imp := range cr.ImportIds {
			if prior, ok := cr.Prior[imp]; ok {
				pid, _ := strconv.ParseInt(prior, 10, 64)
				restore[imp] = [2]int64{ids[i], pid}
			} else {
				drop[imp] = ids[i]
			}
		}
	}

	for _, l := range p.Linked {
		tid, _ := strconv.ParseInt(l.TransactionId, 10, 64)

		if l.Prior != "" {
			pid, _ := strconv.ParseInt(l.Prior, 10, 64)
			restore[l.ImportId] = [2]int64{tid, pid}
		} else {
			drop[l.ImportId] = tid
		}
	}

	if err := ingDeleteRecords(mc, drop); err != nil {
		return NewFail(CodeUpstreamError, "retry POST /undo", "could not remove the import records")
	}

	if err := ingRelinkRecords(mc, restore); err != nil {
		return NewFail(CodeUpstreamError, "retry POST /undo", "could not restore the import records")
	}

	return nil
}

// ingStagingRunLog appends the run to {staging}/_run.log (counts and outcome only) and writes the
// unmapped list, when the run belongs to a statements root
func ingStagingRunLog(plan *ingPlanResult, rep *ingRunReport) {
	defer func() { _ = recover() }()

	if plan.staging == nil {
		return
	}

	if err := plan.staging.Ensure(); err != nil {
		return
	}

	line, _ := json.Marshal(map[string]any{
		"run_id": rep.RunId, "at": rep.FinishedAt, "kind": rep.Kind, "mode": rep.Mode, "outcome": rep.Outcome,
		"created": rep.Created, "linked": rep.Linked, "reimported": rep.Reimported, "journal_id": rep.JournalId,
		"blocked_rows": rep.Totals["blocked_rows"], "already_present": rep.Totals["already_present"],
	})

	_ = plan.staging.AppendLine(ingRunLog, string(line))

	var rows [][]string

	for _, u := range plan.Unmapped {
		rows = append(rows, []string{u.Type, u.Name, strconv.Itoa(u.Rows), strings.Join(u.Accounts, " "), u.Reason, u.FallbackId})
	}

	_ = plan.staging.WriteFile("_unmapped.csv", ingRenderCSV([]string{"type", "name", "rows", "accounts", "reason", "fallback_category_id"}, rows))
}

// ingListRuns reads the run reports of a user, newest first
func ingListRuns(uid int64, limit int) ([]map[string]any, int, error) {
	entries, err := os.ReadDir(ingRunsDir(uid))

	if errors.Is(err, fs.ErrNotExist) {
		return []map[string]any{}, 0, nil
	}

	if err != nil {
		return nil, 0, NewFail(CodeInternal, "check the state directory's permissions", "cannot list run reports")
	}

	var names []string

	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && ingRunIdPattern(strings.TrimSuffix(e.Name(), ".json")) {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}

	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	total := len(names)

	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}

	out := make([]map[string]any, 0, len(names))

	for _, n := range names {
		rep, err := ingLoadRunReport(uid, n)

		if err != nil {
			continue
		}

		out = append(out, map[string]any{
			"run_id": rep.RunId, "kind": rep.Kind, "root": rep.Root, "mode": rep.Mode, "file": rep.File,
			"started_at": rep.StartedAt, "finished_at": rep.FinishedAt, "outcome": rep.Outcome,
			"created": rep.Created, "linked": rep.Linked, "reimported": rep.Reimported, "journal_id": rep.JournalId,
			"accounts": len(rep.Accounts), "to_fallback": len(rep.Fallback),
		})
	}

	return out, total, nil
}
