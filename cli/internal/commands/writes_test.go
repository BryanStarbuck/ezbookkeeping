package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
)

// ---------------------------------------------------------------------------------------------
// pure helpers
// ---------------------------------------------------------------------------------------------

func wrTestLoc(t *testing.T) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation("America/Los_Angeles")

	if err != nil {
		t.Skip("no tzdata")
	}

	return loc
}

func TestWrResolveDate(t *testing.T) {
	loc := wrTestLoc(t)
	// 2026-03-01 06:30 UTC is still 2026-02-28 in Los Angeles: the zone decides what "today" is
	now := time.Date(2026, 3, 1, 6, 30, 0, 0, time.UTC)

	cases := []struct {
		in       string
		mode     wrDateMode
		want     string
		relative bool
	}{
		{"today", wrDatePoint, "2026-02-28", true},
		{"yesterday", wrDatePoint, "2026-02-27", true},
		{"this-month", wrDateStart, "2026-02-01", true},
		{"this-month", wrDateEnd, "2026-02-28", true},
		{"last-month", wrDateStart, "2026-01-01", true},
		{"last-month", wrDateEnd, "2026-01-31", true},
		{"this-year", wrDateStart, "2026-01-01", true},
		{"this-year", wrDateEnd, "2026-12-31", true},
		{"last-year", wrDateStart, "2025-01-01", true},
		{"last-year", wrDateEnd, "2025-12-31", true},
		{"2024-02", wrDateEnd, "2024-02-29", true},
		{"2024-02", wrDateStart, "2024-02-01", true},
		{"2025", wrDateEnd, "2025-12-31", true},
		{"2025-06-15", wrDatePoint, "2025-06-15", false},
		{" TODAY ", wrDatePoint, "2026-02-28", true},
		{"", wrDatePoint, "", false},
	}

	for _, tc := range cases {
		got, rel, err := wrResolveDate(tc.in, tc.mode, loc, now)

		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}

		if got != tc.want || rel != tc.relative {
			t.Errorf("%q mode %d: got %q rel=%v, want %q rel=%v", tc.in, tc.mode, got, rel, tc.want, tc.relative)
		}
	}

	for _, bad := range []string{"2026-02-30", "2026-13", "tomorrow", "26-01-01", "last week"} {
		if _, _, err := wrResolveDate(bad, wrDatePoint, loc, now); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestWrTimeOfDay(t *testing.T) {
	for in, want := range map[string]string{"9:05": "09:05:00", "23:59:59": "23:59:59", "00:00": "00:00:00", "": ""} {
		got, err := wrTimeOfDay(in)

		if err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, err, want)
		}
	}

	for _, bad := range []string{"24:00", "12:60", "noon", "12"} {
		if _, err := wrTimeOfDay(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestWrRefKind(t *testing.T) {
	cases := []struct {
		in    string
		value string
		isId  bool
	}{
		{"3401855937219633152", "3401855937219633152", true},
		{"Northbank Checking", "Northbank Checking", false},
		{"2024", "2024", false},
		{"name:3401855937219633152", "3401855937219633152", false},
		{"id:42", "42", true},
		{"401k", "401k", false},
	}

	for _, tc := range cases {
		v, id := wrRefKind(tc.in)

		if v != tc.value || id != tc.isId {
			t.Errorf("%q: got %q/%v want %q/%v", tc.in, v, id, tc.value, tc.isId)
		}
	}

	body := map[string]any{}
	wrSetRef(body, "account", "Northbank Checking")
	wrSetRef(body, "category", "3401855937219633152")
	wrSetRefs(body, "tag", []string{"Trip", "3401855937219633999", "name:2024"})

	if body["account_name"] != "Northbank Checking" || body["category_id"] != "3401855937219633152" {
		t.Errorf("refs: %v", body)
	}

	if ids, _ := body["tag_ids"].([]string); len(ids) != 1 || ids[0] != "3401855937219633999" {
		t.Errorf("tag_ids: %v", body["tag_ids"])
	}

	if names, _ := body["tag_names"].([]string); len(names) != 2 || names[1] != "2024" {
		t.Errorf("tag_names: %v", body["tag_names"])
	}
}

func TestWrNormalizeTxnRow(t *testing.T) {
	loc := wrTestLoc(t)
	now := time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC)

	row := map[string]any{
		"type": "Expense", "account": "Northbank Checking", "amount": json.Number("1250"),
		"category": "Groceries", "date": "yesterday", "tags": []any{"Trip", "3401855937219633999"}, "comment": "synthetic",
	}

	out, notes, err := wrNormalizeTxnRow(row, loc, now)

	if err != nil {
		t.Fatal(err)
	}

	if out["type"] != "expense" || out["account_name"] != "Northbank Checking" || out["amount"] != int64(1250) || out["category_name"] != "Groceries" || out["date"] != "2026-09-20" {
		t.Errorf("normalized: %v", out)
	}

	if len(notes) != 1 || !strings.Contains(notes[0], "2026-09-20") {
		t.Errorf("notes: %v", notes)
	}

	// a transfer is implied by to_account and needs no category
	out, _, err = wrNormalizeTxnRow(map[string]any{"account": "A Checking", "to_account": "B Card", "amount": "50000", "to_amount": json.Number("46000")}, loc, now)

	if err != nil {
		t.Fatal(err)
	}

	if out["type"] != "transfer" || out["destination_account_name"] != "B Card" || out["destination_amount"] != int64(46000) || out["date"] != "2026-09-21" {
		t.Errorf("transfer: %v", out)
	}

	bad := []struct {
		row  map[string]any
		want string
	}{
		{map[string]any{"type": "expense", "account": "A", "amount": json.Number("12.50"), "category": "C"}, "did you mean"},
		{map[string]any{"type": "expense", "account": "A", "amount": json.Number("1e3"), "category": "C"}, "plain integers"},
		{map[string]any{"type": "expense", "account_id": json.Number("3401855937219633152"), "amount": "1", "category": "C"}, "ids are strings"},
		{map[string]any{"type": "expense", "account": "A", "amount": "1", "category": "C", "payee": "x"}, "unknown key"},
		{map[string]any{"type": "expense", "account": "A", "amount": "1"}, "category is required"},
		{map[string]any{"type": "expense", "account": "A", "amount": "-5", "category": "C"}, "negative"},
		{map[string]any{"type": "balance_modification", "account": "A", "amount": "5"}, "type must be"},
		{map[string]any{"type": "expense", "account": "A", "amount": "5", "category": "C", "to_account": "B"}, "only apply to a transfer"},
		{map[string]any{"type": "transfer", "account": "A", "amount": "5"}, "needs to_account"},
		{map[string]any{"account": "A", "amount": "5", "category": "C"}, "type is required"},
		{map[string]any{"type": "income", "amount": "5", "category": "C"}, "account is required"},
		{map[string]any{"type": "income", "account": "A", "category": "C"}, "amount is required"},
	}

	for _, b := range bad {
		_, _, err := wrNormalizeTxnRow(b.row, loc, now)

		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%v: got %v, want error containing %q", b.row, err, b.want)
		}
	}

	tags := make([]any, 11)

	for i := range tags {
		tags[i] = "t" + string(rune('a'+i))
	}

	if _, _, err := wrNormalizeTxnRow(map[string]any{"type": "expense", "account": "A", "amount": "5", "category": "C", "tags": tags}, loc, now); err == nil || !strings.Contains(err.Error(), "at most 10") {
		t.Errorf("11 tags: %v", err)
	}
}

func TestWrParseRows(t *testing.T) {
	rows, err := wrParseRows([]byte(`[{"type":"expense","amount":1250},{"type":"income","amount":"99"}]`))

	if err != nil || len(rows) != 2 {
		t.Fatalf("array: %v %v", rows, err)
	}

	if _, ok := rows[0]["amount"].(json.Number); !ok {
		t.Errorf("amount must stay a json.Number, got %T", rows[0]["amount"])
	}

	rows, err = wrParseRows([]byte(`{"transactions":[{"type":"expense"}]}`))

	if err != nil || len(rows) != 1 {
		t.Fatalf("wrapped: %v %v", rows, err)
	}

	for _, bad := range []string{`[]`, `{"rows":[]}`, `[1,2]`, `not json`, `"x"`} {
		if _, err := wrParseRows([]byte(bad)); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
}

func TestWrFrequency(t *testing.T) {
	f, v, err := wrFrequency("Every-N-Days", []string{"14"})

	if err != nil || f != "every_n_days" || len(v) != 1 || v[0] != "14" {
		t.Errorf("every-n-days: %q %v %v", f, v, err)
	}

	f, v, err = wrFrequency("weekly", []string{"mon", "fri"})

	if err != nil || f != "weekly" || len(v) != 2 || v[0] != "mon" {
		t.Errorf("weekly passes the day names through for the server to parse: %q %v %v", f, v, err)
	}

	f, v, err = wrFrequency("daily", nil)

	if err != nil || f != "daily" || v == nil || len(v) != 0 {
		t.Errorf("daily: %q %v %v", f, v, err)
	}

	bad := []struct {
		every string
		days  []string
	}{
		{"daily", []string{"1"}},
		{"weekly", nil},
		{"monthly", nil},
		{"every-n-days", []string{"1", "2"}},
		{"fortnightly", []string{"1"}},
	}

	for _, b := range bad {
		if _, _, err := wrFrequency(b.every, b.days); err == nil {
			t.Errorf("%s %v: expected an error", b.every, b.days)
		}
	}
}

func TestWrTimeInstant(t *testing.T) {
	loc := wrTestLoc(t)
	now := time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC)

	out, _, err := wrNormalizeTxnRow(map[string]any{"type": "expense", "account": "A", "amount": "1", "category": "C", "date": "2026-09-01", "time": "8:15"}, loc, now)

	if err != nil {
		t.Fatal(err)
	}

	if out["time"] != "2026-09-01T08:15:00-07:00" || out["date"] != nil {
		t.Errorf("date + HH:MM → RFC 3339 in the zone: %v", out)
	}

	out, _, err = wrNormalizeTxnRow(map[string]any{"type": "expense", "account": "A", "amount": "1", "category": "C", "time": "2026-01-02T03:04:05Z"}, loc, now)

	if err != nil || out["time"] != "2026-01-02T03:04:05Z" || out["date"] != nil {
		t.Errorf("RFC 3339 passes through: %v %v", out, err)
	}

	if _, _, err := wrNormalizeTxnRow(map[string]any{"type": "expense", "account": "A", "amount": "1", "category": "C", "date": "2026-01-02", "time": "2026-01-02T03:04:05Z"}, loc, now); err == nil {
		t.Errorf("date plus an RFC 3339 time must be refused")
	}
}

func TestWrFlatRows(t *testing.T) {
	dry := map[string]any{
		"dry_run": true,
		"changes": map[string]any{"update": json.Number("1")},
		"preview": map[string]any{"rows": []any{
			map[string]any{"id": "9", "date": "2026-09-01", "typeName": "expense", "sourceAccountName": "Checking", "sourceAmount": json.Number("1250"), "sourceCurrency": "USD",
				"changes": []any{
					map[string]any{"field": "categoryId", "from": "1", "to": "2", "fromName": "Food", "toName": "Groceries"},
					map[string]any{"field": "comment", "from": "a", "to": "b"},
				}},
		}},
	}

	rows := wrFlatRows(dry)

	if len(rows) != 2 || rows[0]["from"] != "Food" || rows[0]["to"] != "Groceries" || rows[1]["field"] != "comment" || rows[0]["account"] != "Checking" || rows[0]["currency"] != "USD" {
		t.Errorf("bulk flatten: %v", rows)
	}

	cols := wrFlatColumns(rows)

	for _, c := range cols {
		if c.Key == "toAccount" || c.Key == "name" {
			t.Errorf("unused column %q kept", c.Key)
		}
	}

	hide := map[string]any{"dry_run": true, "preview": []any{map[string]any{"id": "5", "name": "Trip", "field": "hidden", "from": false, "to": true}}}

	if rows := wrFlatRows(hide); len(rows) != 1 || rows[0]["field"] != "hidden" || rows[0]["to"] != true {
		t.Errorf("field-change flatten: %v", rows)
	}

	applied := map[string]any{"dry_run": false, "changes": map[string]any{"create": json.Number("2")}, "journal_id": json.Number("12"), "result": map[string]any{"created": json.Number("2"), "ids": []any{"1", "2"}}}

	if rows := wrFlatRows(applied); len(rows) != 2 || rows[1]["id"] != "2" || !strings.Contains(rows[0]["status"].(string), "journal 12") {
		t.Errorf("applied flatten: %v", rows)
	}

	if rows := wrFlatRows(map[string]any{"dry_run": true, "changes": map[string]any{}}); len(rows) != 1 || !strings.Contains(rows[0]["status"].(string), "no changes") {
		t.Errorf("empty preview: %v", rows)
	}
}

func TestWrRate(t *testing.T) {
	for _, ok := range []string{"1.0842", "150", "0.0067"} {
		if got, err := wrRate(ok); err != nil || got != ok {
			t.Errorf("%q: %q %v", ok, got, err)
		}
	}

	for _, bad := range []string{"0", "0.000", "-1.2", "1,08", "1e3", "abc", ""} {
		if _, err := wrRate(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestWrCurrency(t *testing.T) {
	if v, err := wrCurrency(" eur "); err != nil || v != "EUR" {
		t.Errorf("eur: %q %v", v, err)
	}

	for _, bad := range []string{"EURO", "E1R", "", "us"} {
		if _, err := wrCurrency(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestWrParseSubAccount(t *testing.T) {
	sub, err := wrParseSubAccount("Brokerage Cash:USD:125000")

	if err != nil || sub["name"] != "Brokerage Cash" || sub["currency"] != "USD" || sub["initial_balance"] != int64(125000) {
		t.Errorf("full: %v %v", sub, err)
	}

	sub, err = wrParseSubAccount("Euro Pot:eur")

	if err != nil || sub["currency"] != "EUR" || sub["initial_balance"] != nil {
		t.Errorf("no balance: %v %v", sub, err)
	}

	for _, bad := range []string{":USD", "A:USDX", "A:USD:12.50", "A:B:C:D"} {
		if _, err := wrParseSubAccount(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestWrNameMatches(t *testing.T) {
	rows := []any{
		map[string]any{"id": "1", "name": "Card", "subAccounts": []any{map[string]any{"id": "2", "name": "card"}}},
		map[string]any{"id": "3", "name": "Checking"},
	}

	if m := wrNameMatches(rows, "Card"); len(m) != 1 || m[0]["id"] != "1" {
		t.Errorf("exact wins over folded: %v", m)
	}

	if m := wrNameMatches(rows, "CHECKING"); len(m) != 1 || m[0]["id"] != "3" {
		t.Errorf("folded: %v", m)
	}

	if m := wrNameMatches(rows, "CARD"); len(m) != 2 {
		t.Errorf("ambiguous folded: %v", m)
	}

	if m := wrNameMatches(rows, "Savings"); len(m) != 0 {
		t.Errorf("none: %v", m)
	}
}

func TestWrNextJournalEntry(t *testing.T) {
	data := map[string]any{
		"entries": []any{
			map[string]any{"journalId": "9", "route": "POST /tags", "undone": true},
			map[string]any{"journalId": "8", "route": "POST /transactions", "undone": false},
		},
		"nextUndo": "8",
		"nextRedo": "9",
	}

	if e := wrNextJournalEntry(data, "undo"); e == nil || e["route"] != "POST /transactions" {
		t.Errorf("undo: %v", e)
	}

	if e := wrNextJournalEntry(data, "redo"); e == nil || e["route"] != "POST /tags" {
		t.Errorf("redo: %v", e)
	}

	data["nextUndo"] = "3"

	if e := wrNextJournalEntry(data, "undo"); e == nil || e["journalId"] != "3" {
		t.Errorf("stub: %v", e)
	}

	data["nextRedo"] = nil

	if e := wrNextJournalEntry(data, "redo"); e != nil {
		t.Errorf("no redo: %v", e)
	}
}

// ---------------------------------------------------------------------------------------------
// flag declarations
// ---------------------------------------------------------------------------------------------

func TestWrVerbsDeclared(t *testing.T) {
	want := []string{
		"transactions add", "transactions edit", "transactions set-category", "transactions set-account",
		"transactions tag add", "transactions tag remove", "transactions tag clear",
		"accounts add", "accounts edit", "accounts hide", "accounts unhide", "accounts reconcile",
		"categories add", "categories edit", "categories hide", "categories unhide",
		"tags add", "tags edit", "tags hide", "tags unhide",
		"schedules add", "schedules edit", "schedules pause", "schedules resume",
		"rates set", "rates clear", "undo", "redo", "journal",
	}

	for _, name := range want {
		v, n := app.Lookup(strings.Fields(name))

		if v == nil || v.Name != name || n != len(strings.Fields(name)) {
			t.Errorf("verb %q is not registered", name)
			continue
		}

		if v.Group != wrGroup {
			t.Errorf("verb %q is in group %q", name, v.Group)
		}

		seen := map[string]bool{}

		for _, f := range v.Flags {
			if seen[f.Name] {
				t.Errorf("verb %q declares --%s twice", name, f.Name)
			}

			seen[f.Name] = true

			for _, u := range app.UniversalFlags {
				if u.Name == f.Name {
					t.Errorf("verb %q redeclares universal flag --%s", name, f.Name)
				}
			}
		}
	}
}

func TestWrBulkFilterRename(t *testing.T) {
	names := func(target wrTarget) map[string]bool {
		m := map[string]bool{}

		for _, f := range wrBulkFlags(target) {
			m[f.Name] = true
		}

		return m
	}

	sc := names(wrTargetCategory)

	if !sc["category"] || !sc["where-category"] || !sc["account"] || sc["where-account"] {
		t.Errorf("set-category flags: %v", sc)
	}

	sa := names(wrTargetAccount)

	if !sa["account"] || !sa["where-account"] || !sa["category"] {
		t.Errorf("set-account flags: %v", sa)
	}

	tg := names(wrTargetTags)

	if !tg["tag"] || !tg["where-tag"] || !tg["untagged"] {
		t.Errorf("tag flags: %v", tg)
	}

	cl := names(wrTargetNone)

	if !cl["tag"] || cl["where-tag"] {
		t.Errorf("tag clear flags: %v", cl)
	}
}

// ---------------------------------------------------------------------------------------------
// end to end against a fake machine plane: the write protocol and the exact bodies sent
// ---------------------------------------------------------------------------------------------

type wrCall struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

type wrFake struct {
	t     *testing.T
	mu    sync.Mutex
	calls []wrCall
	// answer returns the data for a call, or an error envelope when code != ""
	answer func(c wrCall) (data any, code string)
}

func (f *wrFake) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.URL.Path == "/healthz.json" {
		_, _ = io.WriteString(w, `{"status":"ok","version":"test"}`)
		return
	}

	if r.URL.Path == "/machine/v1/ping" {
		_, _ = io.WriteString(w, `{"ok":true,"data":{"pong":true}}`)
		return
	}

	raw, _ := io.ReadAll(r.Body)
	call := wrCall{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/machine/v1"), Query: r.URL.RawQuery}

	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()

		if err := dec.Decode(&call.Body); err != nil {
			f.t.Errorf("body is not a JSON object: %s", raw)
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	data, code := f.answer(call)

	if code != "" {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]any{"code": code, "message": "refused by the fake", "hint": "fake hint"}})

		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data, "meta": map[string]any{"timezone": "UTC"}})
}

// wrWriteAnswer is the default fake for a RunWrite route
func wrWriteAnswer(c wrCall) (any, string) {
	if c.Body != nil && c.Body["dry_run"] == false {
		return map[string]any{"dry_run": false, "changes": map[string]int{"create": 1}, "result": map[string]any{"ok": true}}, ""
	}

	return map[string]any{"dry_run": true, "changes": map[string]int{"create": 1}, "preview": []any{}, "confirm_token": "cf_test", "fingerprint": "sha256:x"}, ""
}

func wrRun(t *testing.T, fake *wrFake, args ...string) (int, string, string, []wrCall) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("EZBK_API_KEY", strings.Repeat("ab", 32))
	t.Setenv("EZBK_CREDENTIALS_FILE", dir+"/creds.json")
	t.Setenv("EZBK_STATE_DIR", dir)
	t.Setenv("EZBK_API_URL", srv.URL)

	var out, errOut bytes.Buffer
	code := app.Main(append(args, "--tz", "UTC"), &out, &errOut)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	return code, out.String(), errOut.String(), append([]wrCall(nil), fake.calls...)
}

func TestWrTxnAddDryRunAndWrite(t *testing.T) {
	fake := &wrFake{t: t, answer: wrWriteAnswer}

	code, out, stderr, calls := wrRun(t, fake, "transactions", "add", "--account", "Northbank Checking", "--type", "expense", "--amount", "1250", "--category", "Groceries", "--date", "2026-09-01", "--tag", "Trip")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	if len(calls) != 1 || calls[0].Method != "POST" || calls[0].Path != "/transactions" || calls[0].Body["dry_run"] != true {
		t.Fatalf("dry run calls: %+v", calls)
	}

	rows, _ := calls[0].Body["transactions"].([]any)

	if len(rows) != 1 {
		t.Fatalf("rows: %v", calls[0].Body)
	}

	row := rows[0].(map[string]any)

	if row["account_name"] != "Northbank Checking" || row["amount"] != json.Number("1250") || row["category_name"] != "Groceries" || row["date"] != "2026-09-01" || row["type"] != "expense" {
		t.Errorf("row: %v", row)
	}

	if !strings.Contains(stderr, "DRY RUN") || !strings.Contains(out, "cf_test") {
		t.Errorf("dry run output: stdout %q stderr %q", out, stderr)
	}

	fake.calls = nil
	code, _, stderr, calls = wrRun(t, fake, "transactions", "add", "--account", "Northbank Checking", "--type", "expense", "--amount", "1250", "--category", "Groceries", "--write")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	if len(calls) != 2 || calls[1].Body["dry_run"] != false || calls[1].Body["confirm_token"] != "cf_test" {
		t.Fatalf("write calls: %+v", calls)
	}
}

func TestWrTxnAddRefusesDecimal(t *testing.T) {
	fake := &wrFake{t: t, answer: wrWriteAnswer}
	code, _, stderr, calls := wrRun(t, fake, "transactions", "add", "--account", "A", "--type", "expense", "--amount", "12.50", "--category", "C")

	if code != exitcode.Usage || len(calls) != 0 || !strings.Contains(stderr, "1250") {
		t.Errorf("exit %d calls %d stderr %q", code, len(calls), stderr)
	}
}

func TestWrSetCategoryByFilter(t *testing.T) {
	fake := &wrFake{t: t, answer: wrWriteAnswer}
	code, _, stderr, calls := wrRun(t, fake, "transactions", "set-category", "--category", "Groceries", "--where-category", "Uncategorized Expense", "--account", "Northbank Checking", "--start", "2026-08", "--end", "2026-08", "--min", "500")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	body := calls[0].Body
	filter, _ := body["filter"].(map[string]any)

	if calls[0].Path != "/transactions/set-category" || body["category_name"] != "Groceries" || body["ids"] != nil {
		t.Errorf("body: %v", body)
	}

	if filter["start"] != "2026-08-01" || filter["end"] != "2026-08-31" || filter["min_amount"] != json.Number("500") {
		t.Errorf("filter dates/amounts: %v", filter)
	}

	if names, _ := filter["category_names"].([]any); len(names) != 1 || names[0] != "Uncategorized Expense" {
		t.Errorf("filter categories: %v", filter)
	}

	if names, _ := filter["account_names"].([]any); len(names) != 1 {
		t.Errorf("filter accounts: %v", filter)
	}
}

func TestWrBulkSelectionRules(t *testing.T) {
	fake := &wrFake{t: t, answer: wrWriteAnswer}

	code, _, _, calls := wrRun(t, fake, "transactions", "set-account", "--account", "B")

	if code != exitcode.Usage || len(calls) != 0 {
		t.Errorf("no selection: exit %d calls %d", code, len(calls))
	}

	code, _, _, calls = wrRun(t, fake, "transactions", "tag", "add", "3401855937219633152", "--tag", "Trip", "--keyword", "x")

	if code != exitcode.Usage || len(calls) != 0 {
		t.Errorf("ids and filter: exit %d calls %d", code, len(calls))
	}

	fake.calls = nil
	code, _, stderr, calls := wrRun(t, fake, "transactions", "tag", "add", "3401855937219633152,3401855937219633153", "--tag", "Trip")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	if ids, _ := calls[0].Body["ids"].([]any); len(ids) != 2 || calls[0].Path != "/transactions/tags/add" {
		t.Errorf("ids: %+v", calls[0])
	}

	code, _, _, _ = wrRun(t, fake, "transactions", "tag", "clear", "Groceries")

	if code != exitcode.Usage {
		t.Errorf("a name is not an id: exit %d", code)
	}
}

func TestWrReconcileRefusesDifference(t *testing.T) {
	fake := &wrFake{t: t, answer: func(c wrCall) (any, string) {
		if strings.HasSuffix(c.Path, "/reconcile/plan") {
			return map[string]any{"currency": "USD", "appBalance": 421000, "targetBalance": 421108, "difference": 108, "transactionsSinceLastReconciled": []any{}, "sinceCount": 0}, ""
		}

		return wrWriteAnswer(c)
	}}

	code, _, stderr, calls := wrRun(t, fake, "accounts", "reconcile", "3401855937219633152", "--balance", "421108", "--as-of", "2026-08-31", "--write")

	if code != exitcode.Conflict || !strings.Contains(stderr, "--adjust-into") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}

	for _, c := range calls {
		if strings.HasSuffix(c.Path, "/reconcile/apply") {
			t.Errorf("apply was called despite a difference: %+v", c)
		}
	}

	fake.calls = nil
	code, _, stderr, calls = wrRun(t, fake, "accounts", "reconcile", "3401855937219633152", "--balance", "421108", "--as-of", "2026-08-31", "--adjust-into", "Adjustments", "--write")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	last := calls[len(calls)-1]

	if !strings.HasSuffix(last.Path, "/reconcile/apply") || last.Body["create_adjustment"] != true || last.Body["adjustment_category_name"] != "Adjustments" || last.Body["confirm_token"] != "cf_test" {
		t.Errorf("apply: %+v", last)
	}
}

func TestWrUndoGuardsTheEntryShown(t *testing.T) {
	fake := &wrFake{t: t, answer: func(c wrCall) (any, string) {
		switch c.Path {
		case "/journal":
			return map[string]any{"entries": []any{map[string]any{"journalId": "77", "route": "POST /transactions", "changes": 3, "summary": "added 3", "undone": false}}, "nextUndo": "77", "nextRedo": nil}, ""
		case "/undo":
			return map[string]any{"undone": true, "entry": map[string]any{"journalId": "77", "undone": true}, "redoAvailable": true}, ""
		}

		return nil, "not_found"
	}}

	code, out, stderr, calls := wrRun(t, fake, "undo")

	if code != 0 || len(calls) != 1 || calls[0].Path != "/journal" || !strings.Contains(stderr, "DRY RUN") || !strings.Contains(out, `"journalId": "77"`) {
		t.Fatalf("dry run: exit %d calls %+v stdout %q stderr %q", code, calls, out, stderr)
	}

	fake.calls = nil
	code, _, stderr, calls = wrRun(t, fake, "undo", "--write")

	if code != 0 || len(calls) != 2 || calls[1].Path != "/undo" || calls[1].Body["journal_id"] != "77" {
		t.Fatalf("write: exit %d calls %+v stderr %q", code, calls, stderr)
	}

	fake.calls = nil
	code, _, _, _ = wrRun(t, fake, "redo")

	if code != exitcode.NotFound {
		t.Errorf("nothing to redo: exit %d", code)
	}
}

func TestWrRatesSetAndAccountEditCurrency(t *testing.T) {
	fake := &wrFake{t: t, answer: wrWriteAnswer}
	code, _, stderr, calls := wrRun(t, fake, "rates", "set", "eur", "1.0842")

	if code != 0 || calls[0].Method != "PUT" || calls[0].Path != "/exchange-rates/custom/EUR" || calls[0].Body["rate"] != "1.0842" {
		t.Fatalf("rates set: exit %d %+v %s", code, calls, stderr)
	}

	code, _, _, _ = wrRun(t, fake, "accounts", "edit", "3401855937219633152", "--currency", "EUR")

	if code != exitcode.Usage {
		t.Errorf("currency edit must be refused locally: exit %d", code)
	}
}

func TestWrTableFlattensBulkPreview(t *testing.T) {
	fake := &wrFake{t: t, answer: func(c wrCall) (any, string) {
		return map[string]any{
			"dry_run": true, "changes": map[string]int{"update": 1}, "confirm_token": "cf_test",
			"preview": map[string]any{"selectedBy": "ids", "rows": []any{map[string]any{
				"id": "3401855937219633152", "date": "2026-09-01", "typeName": "expense", "sourceAccountName": "Checking",
				"sourceAmount": 1250, "sourceCurrency": "USD",
				"changes": []any{map[string]any{"field": "categoryId", "from": "1", "to": "2", "fromName": "Food", "toName": "Groceries"}},
			}}},
		}, ""
	}}

	code, out, stderr, _ := wrRun(t, fake, "transactions", "set-category", "3401855937219633152", "--category", "Groceries", "--format", "table")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	for _, want := range []string{"Groceries", "Food", "$12.50", "categoryId", "Checking"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "cf_test") || strings.Contains(out, `"ok"`) {
		t.Errorf("table output must not carry the envelope:\n%s", out)
	}

	code, out, _, _ = wrRun(t, fake, "transactions", "set-category", "3401855937219633152", "--category", "Groceries", "--format", "csv")

	if code != 0 || !strings.HasPrefix(out, "status,id,date,type,account,amount_hundredths,amount,amount_currency") || !strings.Contains(out, "1250,12.50,USD") {
		t.Errorf("csv:\n%s", out)
	}
}

func TestWrAccountAddPrintsServerVerdict(t *testing.T) {
	fake := &wrFake{t: t, answer: func(c wrCall) (any, string) {
		if c.Body["dry_run"] == false {
			return map[string]any{"dry_run": false, "changes": map[string]int{"create": 1}, "result": map[string]any{"id": "3401855937219630001", "name": "Card"}}, ""
		}

		return map[string]any{"dry_run": true, "changes": map[string]int{"create": 1}, "confirm_token": "cf_test",
			"preview": map[string]any{"action": "create", "name": "Card", "category": "credit_card", "side": "liability", "currency": "USD"}}, ""
	}}

	code, _, stderr, calls := wrRun(t, fake, "accounts", "add", "--name", "Card", "--category", "credit-card", "--currency", "usd", "--initial-balance", "30000", "--write")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	if !strings.Contains(stderr, "LIABILITY") || !strings.Contains(stderr, "currency USD") || !strings.Contains(stderr, "-30000") {
		t.Errorf("verdict on stderr: %s", stderr)
	}

	last := calls[len(calls)-1]

	if last.Body["category"] != "credit_card" || last.Body["currency"] != "USD" || last.Body["initial_balance"] != json.Number("30000") || last.Body["confirm_token"] != "cf_test" {
		t.Errorf("apply body: %v", last.Body)
	}
}

func TestWrSchedulePauseAndResume(t *testing.T) {
	fake := &wrFake{t: t, answer: func(c wrCall) (any, string) {
		if c.Method == "GET" {
			return map[string]any{"template": map[string]any{"id": "3401855937219630002", "kind": "scheduled", "name": "Rent",
				"schedule": map[string]any{"frequency": "monthly", "frequencyValue": "1,15", "describe": "monthly on the 1st and 15th", "paused": false}}}, ""
		}

		return wrWriteAnswer(c)
	}}

	code, _, stderr, calls := wrRun(t, fake, "schedules", "pause", "Rent")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	if !strings.Contains(stderr, `ezbk schedules resume "Rent" --every monthly --day 1 --day 15`) {
		t.Errorf("resume command on stderr: %s", stderr)
	}

	last := calls[len(calls)-1]

	if last.Method != "PATCH" || last.Path != "/templates/Rent" || last.Body["frequency"] != "disabled" {
		t.Errorf("pause: %+v", last)
	}

	fake.calls = nil
	code, _, _, calls = wrRun(t, fake, "schedules", "resume", "Rent", "--every", "monthly", "--day", "1", "--day", "15")

	if code != 0 {
		t.Fatalf("resume exit %d", code)
	}

	last = calls[len(calls)-1]

	if vals, _ := last.Body["frequency_value"].([]any); last.Body["frequency"] != "monthly" || len(vals) != 2 {
		t.Errorf("resume: %+v", last)
	}

	fake.calls = nil
	code, _, _, _ = wrRun(t, fake, "schedules", "resume", "Rent")

	if code != exitcode.Usage {
		t.Errorf("resume of a running schedule without --every: exit %d", code)
	}
}

func TestWrTxnAddFromFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/rows.json"
	rows := `[{"type":"expense","account":"Checking","amount":1250,"category":"Groceries","date":"2026-09-01"},
	          {"account":"Checking","to_account":"Card","amount":"50000","date":"2026-09-02","time":"09:30"}]`

	if err := writeFileForTest(path, rows); err != nil {
		t.Fatal(err)
	}

	fake := &wrFake{t: t, answer: wrWriteAnswer}
	code, _, stderr, calls := wrRun(t, fake, "transactions", "add", "--from-file", path)

	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	list, _ := calls[0].Body["transactions"].([]any)

	if len(list) != 2 {
		t.Fatalf("rows sent: %v", calls[0].Body)
	}

	second := list[1].(map[string]any)

	if second["type"] != "transfer" || second["time"] != "2026-09-02T09:30:00Z" || second["date"] != nil || second["destination_account_name"] != "Card" {
		t.Errorf("second row: %v", second)
	}

	if code, _, _, _ := wrRun(t, fake, "transactions", "add", "--from-file", path, "--amount", "5"); code != exitcode.Usage {
		t.Errorf("--from-file with row flags: exit %d", code)
	}

	if err := writeFileForTest(path, `[{"type":"expense","account":"A","amount":12.5,"category":"C"}]`); err != nil {
		t.Fatal(err)
	}

	fake.calls = nil

	if code, _, stderr, calls := wrRun(t, fake, "transactions", "add", "--from-file", path); code != exitcode.Usage || len(calls) != 0 || !strings.Contains(stderr, "row 1") {
		t.Errorf("decimal in a file row: exit %d calls %d stderr %s", code, len(calls), stderr)
	}
}

func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
