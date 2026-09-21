package machine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/datastore"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// ---------------------------------------------------------------------------------------------
// shared test helpers (jr prefix: several agents write tests in this package)
// ---------------------------------------------------------------------------------------------

// jrIsolate points every file the plane touches at a temp dir, so no test reaches the real home
func jrIsolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("EZBK_CREDENTIALS_FILE", filepath.Join(dir, "creds", "ezbookkeeping.json"))
	t.Setenv("EZBK_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("EZBK_API_KEY", "")
	t.Setenv("EZBK_API_KEY_FILE", "")
	gin.SetMode(gin.TestMode)

	return dir
}

// jrTestCtx builds a handler context over a synthetic request
func jrTestCtx(t *testing.T, route *RouteDef, method, target string, body []byte, params gin.Params) *Ctx {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Host = "127.0.0.1:8080"

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	gc.Request = req
	gc.Params = params
	config := &settings.Config{HttpPort: 8080, Protocol: settings.SCHEME_HTTP}
	loc, _ := time.LoadLocation("America/Los_Angeles")

	return &Ctx{
		Gin:         gc,
		Web:         core.WrapWebContext(gc, config.TrustedProxyIPs),
		Config:      config,
		Route:       route,
		Uid:         42,
		User:        &models.User{Uid: 42, Username: "operator", DefaultCurrency: "USD"},
		Loc:         loc,
		Client:      "ezbk-test/0.0.0",
		WriteTierOn: true,
	}
}

// jrMemBooks is an in-memory row store the synthetic executors and routes act on
type jrMemBooks struct {
	sync.Mutex
	rows map[string]string
	log  []string
}

func (b *jrMemBooks) get(k string) string {
	b.Lock()
	defer b.Unlock()

	return b.rows[k]
}

func (b *jrMemBooks) set(k, v string) {
	b.Lock()
	defer b.Unlock()
	b.rows[k] = v
	b.log = append(b.log, k+"="+v)
}

var jrTestBooks = &jrMemBooks{rows: map[string]string{}}

type jrSetPayload struct {
	Key string `json:"key"`
	To  string `json:"to"`
}

type jrSetCheck struct {
	Key    string `json:"key"`
	Expect string `json:"expect"`
}

func init() {
	// "jrtest.set" sets key to `to`, refusing with a conflict if the row no longer holds `expect`
	RegisterInverse("jrtest.set", func(mc *Ctx, payload json.RawMessage, check json.RawMessage) error {
		var p jrSetPayload
		var c jrSetCheck

		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}

		if len(check) > 0 {
			if err := json.Unmarshal(check, &c); err != nil {
				return err
			}

			if jrTestBooks.get(c.Key) != c.Expect {
				return Conflict("the row was edited since the write", "row %s moved", c.Key)
			}
		}

		jrTestBooks.set(p.Key, p.To)

		return nil
	})

	RegisterInverse("jrtest.panic", func(mc *Ctx, payload json.RawMessage, check json.RawMessage) error {
		panic("boom")
	})
}

// jrSetOp builds the inverse of "key changed from `from` to `to`": undo sets it back to from if it
// still holds to; redo sets it to to if it still holds from
func jrSetOp(key, from, to string) InverseOp {
	redo := NewInverseOp("jrtest.set", jrSetPayload{Key: key, To: to}, jrSetCheck{Key: key, Expect: from})
	op := NewInverseOp("jrtest.set", jrSetPayload{Key: key, To: from}, jrSetCheck{Key: key, Expect: to})
	op.Redo = &redo

	return op
}

func jrResetBooks(rows map[string]string) {
	jrTestBooks.Lock()
	defer jrTestBooks.Unlock()
	jrTestBooks.rows = rows
	jrTestBooks.log = nil
}

func jrFailCode(t *testing.T, err error) string {
	t.Helper()

	if err == nil {
		t.Fatalf("expected an error, got nil")
	}

	var f *Fail

	if !errors.As(err, &f) {
		t.Fatalf("expected a *Fail, got %T: %v", err, err)
	}

	return f.Code
}

// ---------------------------------------------------------------------------------------------
// undo / redo op runners
// ---------------------------------------------------------------------------------------------

func TestJrUndoRunsInReverseOrder(t *testing.T) {
	jrIsolate(t)
	jrResetBooks(map[string]string{"a": "2", "b": "2"})
	mc := jrTestCtx(t, &RouteDef{Method: "POST", Path: "/undo"}, "POST", "/machine/v1/undo", nil, nil)

	// write 1 changed a 1→2, write 2 changed b 1→2; undo must touch b first
	ops := []InverseOp{jrSetOp("a", "1", "2"), jrSetOp("b", "1", "2")}
	n, err := jrRunUndoOps(mc, ops)

	if err != nil || n != 2 {
		t.Fatalf("undo: n=%d err=%v", n, err)
	}

	if got := strings.Join(jrTestBooks.log, ","); got != "b=1,a=1" {
		t.Fatalf("undo order = %s, want b=1,a=1", got)
	}
}

func TestJrUndoRefusesMovedRowAndRestores(t *testing.T) {
	jrIsolate(t)
	// b was edited in the browser after the write (2 → 9)
	jrResetBooks(map[string]string{"a": "2", "b": "9", "c": "2"})
	mc := jrTestCtx(t, &RouteDef{Method: "POST", Path: "/undo"}, "POST", "/machine/v1/undo", nil, nil)
	ops := []InverseOp{jrSetOp("a", "1", "2"), jrSetOp("b", "1", "2"), jrSetOp("c", "1", "2")}

	n, err := jrRunUndoOps(mc, ops)

	if code := jrFailCode(t, err); code != CodeConflict {
		t.Fatalf("code = %s, want conflict", code)
	}

	if n != 1 {
		t.Fatalf("steps reversed before failure = %d, want 1 (c)", n)
	}

	// c must have been put back (redo), b untouched, a untouched
	if jrTestBooks.get("c") != "2" || jrTestBooks.get("b") != "9" || jrTestBooks.get("a") != "2" {
		t.Fatalf("books after refused undo = %v", jrTestBooks.rows)
	}

	var f *Fail
	errors.As(err, &f)
	d := f.Details.(map[string]any)

	if d["restored"] != true || d["failedStep"] != 1 {
		t.Fatalf("details = %v", d)
	}
}

func TestJrUndoPreflightMissingExecutor(t *testing.T) {
	jrIsolate(t)
	jrResetBooks(map[string]string{"a": "2"})
	mc := jrTestCtx(t, &RouteDef{Method: "POST", Path: "/undo"}, "POST", "/machine/v1/undo", nil, nil)
	ops := []InverseOp{jrSetOp("a", "1", "2"), NewInverseOp("jrtest.nobody-registered-this", map[string]string{}, nil)}

	_, err := jrRunUndoOps(mc, ops)

	if code := jrFailCode(t, err); code != CodeInternal {
		t.Fatalf("code = %s", code)
	}

	if len(jrTestBooks.log) != 0 {
		t.Fatalf("preflight must refuse before any step runs; log = %v", jrTestBooks.log)
	}
}

func TestJrUndoExecutorPanicIsContained(t *testing.T) {
	jrIsolate(t)
	jrResetBooks(map[string]string{})
	mc := jrTestCtx(t, &RouteDef{Method: "POST", Path: "/undo"}, "POST", "/machine/v1/undo", nil, nil)

	_, err := jrRunUndoOps(mc, []InverseOp{NewInverseOp("jrtest.panic", nil, nil)})

	if code := jrFailCode(t, err); code != CodeInternal {
		t.Fatalf("code = %s", code)
	}
}

func TestJrRedoForwardAndRollback(t *testing.T) {
	jrIsolate(t)
	jrResetBooks(map[string]string{"a": "1", "b": "1"})
	mc := jrTestCtx(t, &RouteDef{Method: "POST", Path: "/redo"}, "POST", "/machine/v1/redo", nil, nil)
	ops := []InverseOp{jrSetOp("a", "1", "2"), jrSetOp("b", "1", "2")}

	n, err := jrRunRedoOps(mc, ops)

	if err != nil || n != 2 || jrTestBooks.get("a") != "2" || jrTestBooks.get("b") != "2" {
		t.Fatalf("redo: n=%d err=%v rows=%v", n, err, jrTestBooks.rows)
	}

	if got := strings.Join(jrTestBooks.log, ","); got != "a=2,b=2" {
		t.Fatalf("redo order = %s, want forward", got)
	}

	// b moved after the undo → redo refuses and a is undone again
	jrResetBooks(map[string]string{"a": "1", "b": "7"})
	_, err = jrRunRedoOps(mc, ops)

	if code := jrFailCode(t, err); code != CodeConflict {
		t.Fatalf("code = %s", code)
	}

	if jrTestBooks.get("a") != "1" {
		t.Fatalf("a should be rolled back to 1, rows = %v", jrTestBooks.rows)
	}
}

func TestJrRedoNeedsRedoSteps(t *testing.T) {
	ops := []InverseOp{NewInverseOp("jrtest.set", jrSetPayload{Key: "a", To: "1"}, nil)}

	if code := jrFailCode(t, jrPreflightRedo(ops)); code != CodeConflict {
		t.Fatalf("code = %s", code)
	}
}

func TestJrEntryViewNeverCarriesPayload(t *testing.T) {
	ops := []InverseOp{jrSetOp("amount-row", "12500", "99900")}
	data, _ := json.Marshal(ops)
	entry := &MachineJournal{JournalId: 9007199254740993, Route: "PATCH /transactions/:id", Changes: 1, Inverse: string(data), CreatedUnixTime: 1790000000}
	decoded, err := jrDecodeOps(entry)

	if err != nil {
		t.Fatal(err)
	}

	view := jrEntryView(entry, decoded)
	out, _ := json.Marshal(view)

	if bytes.Contains(out, []byte("12500")) || bytes.Contains(out, []byte("99900")) {
		t.Fatalf("journal view leaks payload values: %s", out)
	}

	if view["journalId"] != "9007199254740993" {
		t.Fatalf("journalId must be a decimal string beyond 2^53, got %v", view["journalId"])
	}

	if view["redoable"] != true {
		t.Fatalf("redoable = %v", view["redoable"])
	}
}

func TestJrDecodeOpsMalformed(t *testing.T) {
	if _, err := jrDecodeOps(&MachineJournal{JournalId: 1, Inverse: "{not json"}); err == nil {
		t.Fatal("malformed inverse must be refused")
	}

	ops, err := jrDecodeOps(&MachineJournal{JournalId: 1, Inverse: ""})

	if err != nil || len(ops) != 0 {
		t.Fatalf("empty inverse: %v %v", ops, err)
	}
}

func TestJrClampLimit(t *testing.T) {
	cases := []struct {
		in      int64
		want    int
		clamped bool
	}{{0, 200, false}, {-5, 200, false}, {1, 1, false}, {5000, 5000, false}, {5001, 5000, true}, {1 << 40, 5000, true}}

	for _, c := range cases {
		got, clamped := jrClampLimit(c.in, DefaultLimit, MaxLimit)

		if got != c.want || clamped != c.clamped {
			t.Errorf("jrClampLimit(%d) = %d,%t want %d,%t", c.in, got, clamped, c.want, c.clamped)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// batch: pure helpers
// ---------------------------------------------------------------------------------------------

func TestJrMatchPath(t *testing.T) {
	cases := []struct {
		tmpl, path string
		params     map[string]string
		ok         bool
		want       string
	}{
		{"/transactions", "/transactions", nil, true, ""},
		{"/transactions/:id", "/transactions/9007199254740993", nil, true, "id=9007199254740993"},
		{"/transactions/:id", "/transactions/:id", map[string]string{"id": "77"}, true, "id=77"},
		{"/transactions/:id", "/transactions/:id", nil, false, ""},
		{"/transactions/:id", "/transactions/:other", map[string]string{"other": "1"}, false, ""},
		{"/transactions/:id", "/transactions", nil, false, ""},
		{"/transactions/:id", "/transactions/1/extra", nil, false, ""},
		{"/accounts/:id/reconcile/apply", "/accounts/5/reconcile/apply", nil, true, "id=5"},
		{"/accounts/:id/reconcile/apply", "/accounts/5/reconcile/plan", nil, false, ""},
		{"/tags", "/categories", nil, false, ""},
	}

	for _, c := range cases {
		params, ok := jrMatchPath(c.tmpl, c.path, c.params)

		if ok != c.ok {
			t.Errorf("jrMatchPath(%q, %q) ok = %t, want %t", c.tmpl, c.path, ok, c.ok)
			continue
		}

		var parts []string

		for _, p := range params {
			parts = append(parts, p.Key+"="+p.Value)
		}

		if got := strings.Join(parts, "&"); ok && got != c.want {
			t.Errorf("jrMatchPath(%q, %q) params = %s, want %s", c.tmpl, c.path, got, c.want)
		}
	}
}

func TestJrConcretePath(t *testing.T) {
	got := jrConcretePath("/accounts/:id/reconcile/apply", gin.Params{{Key: "id", Value: "123"}})

	if got != "/accounts/123/reconcile/apply" {
		t.Fatalf("got %s", got)
	}
}

func TestJrCountChanges(t *testing.T) {
	got := jrCountChanges(map[string]int{"create": 2, "update": 46, "unchanged": 3, "Skipped": 9, "duplicates": 4, "delete": 1})

	if got != 49 {
		t.Fatalf("count = %d, want 49", got)
	}
}

// jrWithTestRoutes adds a synthetic route family for the duration of a test
func jrWithTestRoutes(t *testing.T, fn func() []RouteDef) {
	t.Helper()
	saved := routeSources
	routeSources = append(append([]func() []RouteDef{}, routeSources...), fn)
	cachedRoutes = nil

	t.Cleanup(func() {
		routeSources = saved
		cachedRoutes = nil
	})
}

type jrTestSetRequest struct {
	WriteOpts
	Key   string `json:"key"`
	Value string `json:"value"`
}

// jrTestSetHandler is a minimal write route in the plane's own protocol
func jrTestSetHandler(mc *Ctx) (any, error) {
	var req jrTestSetRequest

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	key := mc.Param("key")

	if key == "" {
		key = req.Key
	}

	if key == "forbidden" {
		return nil, Invalid("pick another key", "key %s is refused", key)
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		before := jrTestBooks.get(key)
		changes := map[string]int{"update": 1}

		if before == req.Value {
			changes = map[string]int{"unchanged": 1}
		}

		return &Plan{Changes: changes, Count: jrCountChanges(changes), Preview: map[string]any{"key": key, "from": before, "to": req.Value}}, nil
	}, func(p *Plan) (any, error) {
		jrTestBooks.set(key, req.Value)

		return map[string]any{"key": key}, nil
	})
}

func jrTestFamily() []RouteDef {
	return []RouteDef{
		{Method: "POST", Path: "/jrtest/set", Tier: TierWrite, DryRunnable: true, Handler: jrTestSetHandler},
		{Method: "PATCH", Path: "/jrtest/rows/:key", Tier: TierWrite, DryRunnable: true, Handler: jrTestSetHandler},
		{Method: "POST", Path: "/jrtest/not-dry-runnable", Tier: TierWrite, Handler: jrTestSetHandler},
		{Method: "DELETE", Path: "/jrtest/rows/:key", Tier: TierAdmin, Handler: jrTestSetHandler},
		{Method: "GET", Path: "/jrtest/rows", Tier: TierRead, Handler: jrTestSetHandler},
		planned("POST", "/jrtest/planned", TierWrite, "P9", "A planned write."),
	}
}

func TestJrBatchOpSetIsClosed(t *testing.T) {
	jrWithTestRoutes(t, jrTestFamily)
	names := map[string]bool{}

	for _, r := range jrBatchOps() {
		names[jrOpName(r)] = true
	}

	for _, want := range []string{"POST /jrtest/set", "PATCH /jrtest/rows/:key"} {
		if !names[want] {
			t.Errorf("%s should be batchable", want)
		}
	}

	for _, not := range []string{"POST /jrtest/not-dry-runnable", "DELETE /jrtest/rows/:key", "GET /jrtest/rows", "POST /jrtest/planned", "POST /batch", "POST /undo", "POST /redo", "POST /api/*path"} {
		if names[not] {
			t.Errorf("%s must not be batchable", not)
		}
	}
}

func TestJrValidateBatch(t *testing.T) {
	jrWithTestRoutes(t, jrTestFamily)

	bad := []jrBatchRequest{
		{},
		{Operations: []jrBatchOperation{{Op: "POST"}}},
		{Operations: []jrBatchOperation{{Op: "POST /jrtest/nope"}}},
		{Operations: []jrBatchOperation{{Op: "DELETE /jrtest/rows/a"}}},
		{Operations: []jrBatchOperation{{Op: "POST /jrtest/set", Args: json.RawMessage(`[1,2]`)}}},
		{Operations: []jrBatchOperation{{Op: "POST /jrtest/set", Args: json.RawMessage(`{"dry_run": false}`)}}},
		{Operations: []jrBatchOperation{{Op: "POST /jrtest/set", Args: json.RawMessage(`{"confirm_token": "cf_x"}`)}}},
		{Operations: []jrBatchOperation{{Op: "PATCH /jrtest/rows/:key"}}},
	}

	for i, req := range bad {
		if _, err := jrValidateBatch(&req); err == nil || jrFailCode(t, err) != CodeInvalidInput {
			t.Errorf("case %d: expected invalid_input, got %v", i, err)
		}
	}

	many := jrBatchRequest{}

	for i := 0; i <= jrMaxBatchOps; i++ {
		many.Operations = append(many.Operations, jrBatchOperation{Op: "POST /jrtest/set"})
	}

	if _, err := jrValidateBatch(&many); err == nil {
		t.Error("an oversized batch must be refused")
	}

	good := jrBatchRequest{Operations: []jrBatchOperation{
		{Op: "post /jrtest/set", Args: json.RawMessage(`{"key":"a","value":"1"}`)},
		{Op: "PATCH /machine/v1/jrtest/rows/b", Args: json.RawMessage(`{"value":"2"}`)},
		{Op: "PATCH /jrtest/rows/:key", Params: map[string]string{"key": "c"}, Args: json.RawMessage(`{"value":"3"}`)},
	}}
	ops, err := jrValidateBatch(&good)

	if err != nil {
		t.Fatalf("valid batch refused: %v", err)
	}

	if ops[1].path != "/jrtest/rows/b" || ops[2].path != "/jrtest/rows/c" {
		t.Fatalf("paths = %s, %s", ops[1].path, ops[2].path)
	}
}

func jrBatchCall(t *testing.T, body any, writeTier bool) (any, error) {
	t.Helper()
	data, _ := json.Marshal(body)
	route := &RouteDef{Method: "POST", Path: "/batch", Tier: TierWrite, DryRunnable: true}
	mc := jrTestCtx(t, route, "POST", "/machine/v1/batch", data, nil)
	mc.WriteTierOn = writeTier

	return jrHandleBatch(mc)
}

func TestJrBatchDryRunThenApply(t *testing.T) {
	jrIsolate(t)
	jrWithTestRoutes(t, jrTestFamily)
	jrResetBooks(map[string]string{"a": "0", "b": "0", "c": "5"})

	ops := []map[string]any{
		{"op": "POST /jrtest/set", "args": map[string]string{"key": "a", "value": "1"}},
		{"op": "PATCH /jrtest/rows/:key", "params": map[string]string{"key": "b"}, "args": map[string]string{"value": "2"}},
		{"op": "PATCH /jrtest/rows/c", "args": map[string]string{"value": "5"}},
	}

	res, err := jrBatchCall(t, map[string]any{"operations": ops}, false)

	if err != nil {
		t.Fatalf("dry run: %v", err)
	}

	dry := res.(*WriteResult)

	if !dry.DryRun || dry.ConfirmToken == "" || dry.Changes["update"] != 2 || dry.Changes["unchanged"] != 1 {
		t.Fatalf("dry run result = %+v", dry)
	}

	if len(jrTestBooks.log) != 0 {
		t.Fatalf("a dry run changed the books: %v", jrTestBooks.log)
	}

	// applying with the write tier off is refused even with a token
	if _, err := jrBatchCall(t, map[string]any{"operations": ops, "dry_run": false, "confirm_token": dry.ConfirmToken}, false); jrFailCode(t, err) != CodeWriteDisabled {
		t.Fatalf("apply with tier off: %v", err)
	}

	// the refused apply did not consume the token (the tier check comes first), so it still applies
	res, err = jrBatchCall(t, map[string]any{"operations": ops, "dry_run": false, "confirm_token": dry.ConfirmToken}, true)

	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	applied := res.(*WriteResult)
	out := applied.Result.(map[string]any)

	if applied.DryRun || out["applied"] != 3 || out["rolledBack"] != 0 {
		t.Fatalf("apply result = %+v / %v", applied, out)
	}

	if jrTestBooks.get("a") != "1" || jrTestBooks.get("b") != "2" || jrTestBooks.get("c") != "5" {
		t.Fatalf("books = %v", jrTestBooks.rows)
	}

	// the synthetic routes record no journal, so the batch reports them as not undoable
	if nu, ok := out["notUndoable"].([]int); !ok || len(nu) != 3 {
		t.Fatalf("notUndoable = %v", out["notUndoable"])
	}
}

func TestJrBatchStaleAfterBooksMove(t *testing.T) {
	jrIsolate(t)
	jrWithTestRoutes(t, jrTestFamily)
	jrResetBooks(map[string]string{"a": "0"})

	ops := []map[string]any{{"op": "POST /jrtest/set", "args": map[string]string{"key": "a", "value": "1"}}}
	res, err := jrBatchCall(t, map[string]any{"operations": ops}, true)

	if err != nil {
		t.Fatal(err)
	}

	token := res.(*WriteResult).ConfirmToken
	jrTestBooks.set("a", "7") // someone edited the row in the browser

	_, err = jrBatchCall(t, map[string]any{"operations": ops, "dry_run": false, "confirm_token": token}, true)

	if code := jrFailCode(t, err); code != CodeConflict {
		t.Fatalf("code = %s", code)
	}

	if jrTestBooks.get("a") != "7" {
		t.Fatal("a stale batch must not apply")
	}
}

func TestJrBatchPlanFailureNamesTheIndex(t *testing.T) {
	jrIsolate(t)
	jrWithTestRoutes(t, jrTestFamily)
	jrResetBooks(map[string]string{})

	ops := []map[string]any{
		{"op": "POST /jrtest/set", "args": map[string]string{"key": "a", "value": "1"}},
		{"op": "PATCH /jrtest/rows/forbidden", "args": map[string]string{"value": "2"}},
	}

	_, err := jrBatchCall(t, map[string]any{"operations": ops}, true)

	var f *Fail

	if !errors.As(err, &f) || f.Code != CodeInvalidInput {
		t.Fatalf("err = %v", err)
	}

	if d := f.Details.(map[string]any); d["failedIndex"] != 1 || d["phase"] != "plan" {
		t.Fatalf("details = %v", f.Details)
	}
}

func TestJrBatchCeilingCountsChangesNotOperations(t *testing.T) {
	jrIsolate(t)
	jrWithTestRoutes(t, jrTestFamily)
	jrResetBooks(map[string]string{"a": "0", "b": "0", "c": "0"})

	var ops []map[string]any

	for _, k := range []string{"a", "b", "c"} {
		ops = append(ops, map[string]any{"op": "POST /jrtest/set", "args": map[string]string{"key": k, "value": "1"}})
	}

	_, err := jrBatchCall(t, map[string]any{"operations": ops, "max_changes": 2}, true)

	var f *Fail

	if !errors.As(err, &f) || f.Code != CodeConflict {
		t.Fatalf("err = %v", err)
	}

	if d := f.Details.(map[string]any); d["would_change"] != 3 {
		t.Fatalf("the refusal must report the real count, details = %v", d)
	}
}

// ---------------------------------------------------------------------------------------------
// database-backed: the journal, undo, redo and batch end to end on a temp SQLite
// ---------------------------------------------------------------------------------------------

// jrWithDB opens a temp SQLite as the user-data store for one test and restores the previous one
func jrWithDB(t *testing.T) {
	t.Helper()
	dir := jrIsolate(t)
	prevUser, prevToken, prevData := datastore.Container.UserStore, datastore.Container.TokenStore, datastore.Container.UserDataStore

	cfg := &settings.Config{DatabaseConfig: &settings.DatabaseConfig{
		DatabaseType: settings.Sqlite3DbType, DatabasePath: filepath.Join(dir, "jrtest.db"),
		MaxIdleConnection: 1, MaxOpenConnection: 1, ConnectionMaxLifeTime: 60,
	}}

	if err := datastore.InitializeDataStore(cfg); err != nil {
		t.Skipf("sqlite unavailable: %v", err)
	}

	if err := datastore.Container.UserDataStore.SyncStructs(machineTables...); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		datastore.Container.UserStore, datastore.Container.TokenStore, datastore.Container.UserDataStore = prevUser, prevToken, prevData
	})
}

// jrWriteAndJournal simulates a machine-plane write of key from→to, journaled as the route
func jrWriteAndJournal(t *testing.T, uid int64, key, from, to string) int64 {
	t.Helper()
	mc := jrTestCtx(t, &RouteDef{Method: "POST", Path: "/jrtest/set"}, "POST", "/machine/v1/jrtest/set", nil, nil)
	mc.Uid = uid
	jrTestBooks.set(key, to)
	id, err := RecordJournal(mc, "set "+key, 1, []InverseOp{jrSetOp(key, from, to)})

	if err != nil || id == 0 {
		t.Fatalf("RecordJournal: %d %v", id, err)
	}

	return id
}

func jrCallJournalRoute(t *testing.T, h HandlerFunc, method, path, body string, uid int64) (map[string]any, error) {
	t.Helper()
	var data []byte

	if body != "" {
		data = []byte(body)
	}

	mc := jrTestCtx(t, &RouteDef{Method: method, Path: path, Tier: TierWrite}, method, BasePath+path, data, nil)
	mc.Uid = uid
	res, err := h(mc)

	if err != nil {
		return nil, err
	}

	out, _ := Integerize(res)

	return out.(map[string]any), nil
}

func TestJrUndoRedoJournalEndToEnd(t *testing.T) {
	jrWithDB(t)
	jrResetBooks(map[string]string{"a": "1", "b": "1"})

	idA := jrWriteAndJournal(t, 42, "a", "1", "2")
	idB := jrWriteAndJournal(t, 42, "b", "1", "2")
	jrWriteAndJournal(t, 43, "z", "1", "2") // another user's write is invisible here

	j, err := jrCallJournalRoute(t, jrHandleJournal, "GET", "/journal", "", 42)

	if err != nil {
		t.Fatal(err)
	}

	if entries := j["entries"].([]any); len(entries) != 2 || j["nextUndo"] != idString(idB) || j["nextRedo"] != nil {
		t.Fatalf("journal = %v", j)
	}

	// undo reverses the newest first
	u, err := jrCallJournalRoute(t, jrHandleUndo, "POST", "/undo", "{}", 42)

	if err != nil || u["undone"] != true || jrTestBooks.get("b") != "1" || jrTestBooks.get("a") != "2" {
		t.Fatalf("undo 1: %v %v rows=%v", u, err, jrTestBooks.rows)
	}

	// a retried undo guarded by the id it meant answers alreadyUndone instead of undoing a
	u, err = jrCallJournalRoute(t, jrHandleUndo, "POST", "/undo", `{"journal_id":"`+idString(idB)+`"}`, 42)

	if err != nil || u["alreadyUndone"] != true || jrTestBooks.get("a") != "2" {
		t.Fatalf("guarded retry: %v %v", u, err)
	}

	// undo a too, then redo must bring back a first (it was undone last)
	if _, err := jrCallJournalRoute(t, jrHandleUndo, "POST", "/undo", "{}", 42); err != nil || jrTestBooks.get("a") != "1" {
		t.Fatalf("undo 2: %v", err)
	}

	if _, err := jrCallJournalRoute(t, jrHandleUndo, "POST", "/undo", "{}", 42); jrFailCode(t, err) != CodeNotFound {
		t.Fatal("nothing left to undo must be not_found")
	}

	r, err := jrCallJournalRoute(t, jrHandleRedo, "POST", "/redo", "{}", 42)

	if err != nil || r["redone"] != true || jrTestBooks.get("a") != "2" || jrTestBooks.get("b") != "1" {
		t.Fatalf("redo 1: %v %v rows=%v", r, err, jrTestBooks.rows)
	}

	if entry := r["entry"].(map[string]any); entry["journalId"] != idString(idA) {
		t.Fatalf("redo re-applied %v, want %d", entry["journalId"], idA)
	}

	// a new write closes the redo stack
	jrWriteAndJournal(t, 42, "c", "", "3")

	if _, err := jrCallJournalRoute(t, jrHandleRedo, "POST", "/redo", "{}", 42); jrFailCode(t, err) != CodeNotFound {
		t.Fatal("redo after a newer write must be refused")
	}

	// a row edited in the browser blocks its undo, and the entry stays undoable later
	jrTestBooks.set("c", "browser-edit")

	if _, err := jrCallJournalRoute(t, jrHandleUndo, "POST", "/undo", "{}", 42); jrFailCode(t, err) != CodeConflict {
		t.Fatal("undo over a browser edit must be a conflict")
	}

	if jrTestBooks.get("c") != "browser-edit" {
		t.Fatal("undo overwrote a human's later edit")
	}

	j, _ = jrCallJournalRoute(t, jrHandleJournal, "GET", "/journal", "", 42)

	if j["nextUndo"] == nil || j["nextUndo"] == idString(idA) {
		t.Fatalf("the refused entry must still be next: %v", j["nextUndo"])
	}

	// the other user's entry was never touched
	other, _ := jrCallJournalRoute(t, jrHandleJournal, "GET", "/journal", "", 43)

	if len(other["entries"].([]any)) != 1 || jrTestBooks.get("z") != "2" {
		t.Fatalf("user isolation broken: %v", other)
	}

	// limit and include_undone
	mc := jrTestCtx(t, &RouteDef{Method: "GET", Path: "/journal"}, "GET", BasePath+"/journal?limit=1&include_undone=false", nil, nil)
	res, err := jrHandleJournal(mc)

	if err != nil || len(res.(map[string]any)["entries"].([]map[string]any)) != 1 || mc.meta["truncated"] != true {
		t.Fatalf("limited journal: %v %v meta=%v", res, err, mc.meta)
	}
}

// a journaling write route for the batch tests; value "fail-at-apply" plans fine and fails to apply
func jrTestJournaledSet(mc *Ctx) (any, error) {
	var req jrTestSetRequest

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		before := jrTestBooks.get(req.Key)

		return &Plan{Changes: map[string]int{"update": 1}, Count: 1, Preview: map[string]any{"key": req.Key, "from": before, "to": req.Value}, State: before}, nil
	}, func(p *Plan) (any, error) {
		if req.Value == "fail-at-apply" {
			return nil, NewFail(CodeUpstreamError, "retry later", "the service refused")
		}

		before := p.State.(string)
		jrTestBooks.set(req.Key, req.Value)

		if _, err := RecordJournal(mc, "set "+req.Key, 1, []InverseOp{jrSetOp(req.Key, before, req.Value)}); err != nil {
			return nil, err
		}

		return map[string]any{"key": req.Key}, nil
	})
}

func jrJournaledFamily() []RouteDef {
	return []RouteDef{{Method: "POST", Path: "/jrtest/jset", Tier: TierWrite, DryRunnable: true, Handler: jrTestJournaledSet}}
}

func jrApplyBatch(t *testing.T, ops []map[string]any) (any, error) {
	t.Helper()
	res, err := jrBatchCall(t, map[string]any{"operations": ops}, true)

	if err != nil {
		return nil, err
	}

	return jrBatchCall(t, map[string]any{"operations": ops, "dry_run": false, "confirm_token": res.(*WriteResult).ConfirmToken}, true)
}

func TestJrBatchJournalsOnceAndUndoesWhole(t *testing.T) {
	jrWithDB(t)
	jrWithTestRoutes(t, jrJournaledFamily)
	jrResetBooks(map[string]string{"a": "0", "b": "0"})

	res, err := jrApplyBatch(t, []map[string]any{
		{"op": "POST /jrtest/jset", "args": map[string]string{"key": "a", "value": "1"}},
		{"op": "POST /jrtest/jset", "args": map[string]string{"key": "b", "value": "1"}},
	})

	if err != nil {
		t.Fatal(err)
	}

	if res.(*WriteResult).JournalId == 0 {
		t.Fatal("the batch must record one journal entry")
	}

	j, _ := jrCallJournalRoute(t, jrHandleJournal, "GET", "/journal", "", 42)

	if entries := j["entries"].([]any); len(entries) != 1 || fmt.Sprint(entries[0].(map[string]any)["steps"]) != "2" {
		t.Fatalf("journal after batch = %v", j)
	}

	if _, err := jrCallJournalRoute(t, jrHandleUndo, "POST", "/undo", "{}", 42); err != nil {
		t.Fatal(err)
	}

	if jrTestBooks.get("a") != "0" || jrTestBooks.get("b") != "0" {
		t.Fatalf("one undo must reverse the whole batch: %v", jrTestBooks.rows)
	}
}

func TestJrBatchRollsBackOnFailure(t *testing.T) {
	jrWithDB(t)
	jrWithTestRoutes(t, jrJournaledFamily)
	jrResetBooks(map[string]string{"a": "0", "b": "0"})

	_, err := jrApplyBatch(t, []map[string]any{
		{"op": "POST /jrtest/jset", "args": map[string]string{"key": "a", "value": "1"}},
		{"op": "POST /jrtest/jset", "args": map[string]string{"key": "b", "value": "fail-at-apply"}},
	})

	var f *Fail

	if !errors.As(err, &f) || f.Code != CodeUpstreamError {
		t.Fatalf("err = %v", err)
	}

	d := f.Details.(map[string]any)

	if d["failedIndex"] != 1 || d["rolledBack"] != 1 || d["applied"] != 1 {
		t.Fatalf("details = %v", d)
	}

	if jrTestBooks.get("a") != "0" {
		t.Fatalf("operation 0 was not rolled back: %v", jrTestBooks.rows)
	}

	j, _ := jrCallJournalRoute(t, jrHandleJournal, "GET", "/journal", "", 42)

	if len(j["entries"].([]any)) != 0 {
		t.Fatalf("a rolled-back batch leaves no journal entry: %v", j)
	}
}
