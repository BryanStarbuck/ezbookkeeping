package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOrAdminNeedsWriteAndYes(t *testing.T) {
	orTestEnv(t)

	// --write without --yes is refused locally, before any call (nothing listens on port 1)
	for _, argv := range [][]string{
		{"transactions", "delete", "123", "--write"},
		{"accounts", "delete", "123", "--write"},
		{"categories", "delete", "5", "--write"},
		{"admin", "clear-data", "--scope", "all", "--write"},
		{"admin", "sessions", "revoke", "abc", "--write"},
		{"admin", "pictures", "prune", "--write"},
	} {
		_, errOut, code := orRun(t, append(argv, "--api", "http://127.0.0.1:1")...)

		if code != 2 || !strings.Contains(errOut, "--write --yes") {
			t.Errorf("%v: exit %d %q", argv, code, errOut)
		}
	}
}

func TestOrAdminIdsOnly(t *testing.T) {
	orTestEnv(t)

	_, errOut, code := orRun(t, "accounts", "delete", "Northbank Checking", "--api", "http://127.0.0.1:1")

	if code != 2 || !strings.Contains(errOut, "never names") {
		t.Fatalf("a delete by name is refused: %d %q", code, errOut)
	}

	_, _, code = orRun(t, "admin", "clear-data", "--scope", "transactions_of_account", "--api", "http://127.0.0.1:1")

	if code != 2 {
		t.Fatalf("transactions_of_account needs --account: %d", code)
	}

	_, _, code = orRun(t, "admin", "clear-data", "--scope", "everything", "--api", "http://127.0.0.1:1")

	if code != 2 {
		t.Fatalf("unknown scope: %d", code)
	}
}

func TestOrTransactionsDeleteTwoStep(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_API_KEY", orTestKey)

	var bodies []map[string]any
	srv := orTestPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"DELETE /transactions/bulk": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			bodies = append(bodies, m)

			if m["dry_run"] == true {
				orWriteJSON(w, 200, map[string]any{"ok": true, "data": map[string]any{
					"dry_run": true, "changes": map[string]int{"delete": 2}, "confirm_token": "cf_test",
					"preview": map[string]any{"rows": []any{
						map[string]any{"id": "1", "date": "2026-09-01", "typeName": "expense", "sourceAccountName": "Cash", "sourceAmount": 1250, "sourceCurrency": "USD", "categoryName": "Food"},
						map[string]any{"id": "2", "date": "2026-09-02", "typeName": "income", "sourceAccountName": "Cash", "sourceAmount": 500, "sourceCurrency": "USD", "categoryName": "Salary"},
					}},
				}})

				return
			}

			orWriteJSON(w, 200, map[string]any{"ok": true, "data": map[string]any{"dry_run": false, "changes": map[string]int{"delete": 2}, "result": map[string]any{"deleted": 2}}})
		},
	})

	out, errOut, code := orRun(t, "transactions", "delete", "1", "2,1", "--api", srv.URL, "--format", "table")

	if code != 0 || len(bodies) != 1 || bodies[0]["dry_run"] != true {
		t.Fatalf("without --write it is only the dry run: exit %d bodies %v %s", code, bodies, errOut)
	}

	if ids, _ := bodies[0]["ids"].([]any); len(ids) != 2 {
		t.Fatalf("ids are de-duplicated: %v", bodies[0]["ids"])
	}

	if !strings.Contains(out, "$12.50") || !strings.Contains(errOut, "DRY RUN") {
		t.Fatalf("the preview is printed: %q %q", out, errOut)
	}

	bodies = nil
	_, errOut, code = orRun(t, "transactions", "delete", "1", "2", "--api", srv.URL, "--write", "--yes")

	if code != 0 || len(bodies) != 2 || bodies[1]["dry_run"] != false || bodies[1]["confirm_token"] != "cf_test" {
		t.Fatalf("--write --yes echoes the confirm token: exit %d %v %s", code, bodies, errOut)
	}
}

func TestOrAdminTierRefusalIsExit7(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_API_KEY", orTestKey)

	srv := orTestPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"DELETE /categories/5": func(w http.ResponseWriter, r *http.Request) {
			orWriteJSON(w, 403, map[string]any{"ok": false, "error": map[string]any{"code": "forbidden", "message": "the admin tier is off on this server", "hint": "restart the app with admin allowed: ezbk stop && ezbk up --allow-write --allow-admin"}})
		},
	})

	_, errOut, code := orRun(t, "categories", "delete", "5", "--api", srv.URL)

	if code != 7 || !strings.Contains(errOut, "--allow-admin") {
		t.Fatalf("admin tier off is exit 7 naming the fix: %d %q", code, errOut)
	}
}
