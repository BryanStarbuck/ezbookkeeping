package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
)

// Synthetic ids and names only.

func TestXfVerbsRegistered(t *testing.T) {
	for name, group := range map[string]string{"transactions transfer-candidates": orGroupRead, "transactions convert-to-transfer": wrGroup} {
		v, used := app.Lookup(strings.Fields(name))

		if v == nil || v.Name != name || used != len(strings.Fields(name)) || v.Group != group {
			t.Errorf("verb %q not registered in %q (got %v)", name, group, v)
		}
	}
}

func TestXfCandidatesBody(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_API_KEY", orTestKey)

	var got map[string]any
	srv := orTestPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /transactions/transfer-candidates": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			orWriteJSON(w, 200, map[string]any{"ok": true, "data": map[string]any{
				"pairs": []any{map[string]any{
					"expense": map[string]any{"id": "101", "date": "2026-03-02", "accountName": "Northbank Checking ••4021", "amount": 50000, "currency": "USD", "comment": "MERIDIAN CARD x7788"},
					"income":  map[string]any{"id": "201", "date": "2026-03-04", "accountName": "Meridian Card ••7788", "amount": 50000, "currency": "USD"},
					"dayGap":  2, "item": map[string]any{"id": "101", "counter_id": "201"},
				}},
				"ambiguous": []any{}, "counts": map[string]any{"pairs": 1, "ambiguousGroups": 0, "ambiguousCandidates": 0, "scannedExpenses": 3, "scannedIncomes": 2},
			}})
		},
	})

	out, errOut, code := orRun(t, "transactions", "transfer-candidates", "--api", srv.URL, "--format", "table", "--start", "2026-03-01", "--window-days", "6", "--no-hint", "--account", "Northbank Checking ••4021")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if got["start"] != "2026-03-01" || got["window_days"] != float64(6) || got["require_hint"] != false {
		t.Fatalf("body = %v", got)
	}

	if names, _ := got["account_names"].([]any); len(names) != 1 {
		t.Fatalf("an account name travels as account_names: %v", got)
	}

	if !strings.Contains(out, "$500.00") || !strings.Contains(out, "Meridian Card") || !strings.Contains(errOut, "1 unambiguous pairs") {
		t.Fatalf("table = %q / %q", out, errOut)
	}
}

func TestXfConvertTwoStep(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_API_KEY", orTestKey)

	var bodies []map[string]any
	srv := orTestPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /transactions/convert-to-transfer": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			bodies = append(bodies, m)

			if m["dry_run"] == true {
				orWriteJSON(w, 200, map[string]any{"ok": true, "data": map[string]any{"dry_run": true, "changes": map[string]int{"convert": 2, "create": 2, "delete": 3}, "confirm_token": "cf_test", "preview": map[string]any{"items": []any{}}}})
				return
			}

			orWriteJSON(w, 200, map[string]any{"ok": true, "data": map[string]any{"dry_run": false, "changes": map[string]int{"convert": 2}, "result": map[string]any{"converted": 2, "ids": []any{"301", "302"}}}})
		},
	})

	_, errOut, code := orRun(t, "transactions", "convert-to-transfer", "--api", srv.URL, "--pair", "101:201", "--counter", "102=Market Value", "--transfer-category", "Internal", "--comment-mode", "out")

	if code != 0 || len(bodies) != 1 || bodies[0]["dry_run"] != true {
		t.Fatalf("without --write it is only the dry run: exit %d %v %s", code, bodies, errOut)
	}

	items, _ := bodies[0]["items"].([]any)

	if len(items) != 2 || items[0].(map[string]any)["counter_id"] != "201" || items[1].(map[string]any)["counter_account_name"] != "Market Value" {
		t.Fatalf("items = %v", bodies[0]["items"])
	}

	if bodies[0]["transfer_category_name"] != "Internal" || bodies[0]["comment_mode"] != "out" {
		t.Fatalf("body = %v", bodies[0])
	}

	bodies = nil
	_, errOut, code = orRun(t, "transactions", "convert-to-transfer", "--api", srv.URL, "--items", `[{"id":"101","counter_id":"201"}]`, "--write", "--yes")

	if code != 0 || len(bodies) != 2 || bodies[1]["dry_run"] != false || bodies[1]["confirm_token"] != "cf_test" {
		t.Fatalf("--write echoes the confirm token: exit %d %v %s", code, bodies, errOut)
	}

	if _, _, code := orRun(t, "transactions", "convert-to-transfer", "--api", srv.URL); code == 0 {
		t.Fatal("no items is a usage error")
	}

	if _, _, code := orRun(t, "transactions", "convert-to-transfer", "--api", srv.URL, "--pair", "101"); code == 0 {
		t.Fatal("a malformed --pair is a usage error")
	}
}
