package commands

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOrRawNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"accounts/list.json":         "accounts/list.json",
		"/accounts/list.json":        "accounts/list.json",
		"/api/v1/accounts/list.json": "accounts/list.json",
		"api/accounts/list.json":     "accounts/list.json",
		"/machine/v1/api/tags.json":  "tags.json",
	} {
		got, err := orRawNormalizePath(in)

		if err != nil || got != want {
			t.Errorf("%q → %q %v, want %q", in, got, err, want)
		}
	}

	for _, bad := range []string{"", "http://x/api/v1/a.json", "accounts/list.json?x=1", "../etc/passwd", "api/v1/"} {
		if _, err := orRawNormalizePath(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestOrRawClasses(t *testing.T) {
	for _, p := range []string{"tokens/list.json", "users/2fa/enable.json", "llm/x.json", "users/external_auth/list.json", "users/avatar/remove.json", "users/verify_email/resend.json"} {
		if !orRawRefusedPath(p) {
			t.Errorf("%q must be refused locally", p)
		}
	}

	if orRawRefusedPath("transactions/list.json") {
		t.Error("ordinary paths pass")
	}

	for p, want := range map[string]string{"accounts/list.json": "read", "transactions/modify.json": "write", "transactions/delete.json": "admin", "data/clear/all.json": "admin", "transaction/pictures/remove_unused.json": "admin"} {
		method := "POST"

		if want == "read" {
			method = "GET"
		}

		if got := orRawTier(method, p); got != want {
			t.Errorf("%s %s: %s want %s", method, p, got, want)
		}
	}
}

func TestOrRawQueryAndBody(t *testing.T) {
	q, err := orRawQuery([]string{"count=50", "tag_filter=0:1,2", "keyword=a=b"})

	if err != nil || q.Get("count") != "50" || q.Get("tag_filter") != "0:1,2" || q.Get("keyword") != "a=b" {
		t.Fatalf("%v %v", q, err)
	}

	if _, err := orRawQuery([]string{"novalue"}); err == nil {
		t.Fatal("k=v required")
	}

	if b, err := orRawBody(` {"id":"1"} `, nil); err != nil || string(b) != `{"id":"1"}` {
		t.Fatalf("%q %v", b, err)
	}

	if _, err := orRawBody(`{nope`, nil); err == nil {
		t.Fatal("invalid JSON refused")
	}

	if b, err := orRawBody("-", strings.NewReader(`[1,2]`)); err != nil || string(b) != "[1,2]" {
		t.Fatalf("stdin: %q %v", b, err)
	}
}

func TestOrRawWriteWarnsAndForwards(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_API_KEY", orTestKey)

	var gotBody string
	srv := orTestPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/transactions/modify.json": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			orWriteJSON(w, 200, map[string]any{"ok": true, "data": true, "meta": map[string]any{"passthrough": true}})
		},
		"GET /api/accounts/list.json": func(w http.ResponseWriter, r *http.Request) {
			orWriteJSON(w, 200, map[string]any{"ok": true, "data": []any{map[string]any{"id": "1", "name": "Cash", "balance": "100"}}, "meta": map[string]any{"passthrough": true}})
		},
	})

	out, errOut, code := orRun(t, "raw", "POST", "/api/v1/transactions/modify.json", "--api", srv.URL, "--data", `{"id":"5","comment":"x"}`)

	if code != 0 || gotBody != `{"id":"5","comment":"x"}` {
		t.Fatalf("exit %d body %q: %s", code, gotBody, errOut)
	}

	if !strings.Contains(errOut, "warning:") || !strings.Contains(errOut, "journal") {
		t.Fatalf("every raw write prints the warning: %q", errOut)
	}

	if strings.Contains(out, "warning") {
		t.Fatal("the warning belongs on stderr")
	}

	out, errOut, code = orRun(t, "raw", "GET", "accounts/list.json", "--api", srv.URL, "--format", "table")

	if code != 0 || strings.Contains(errOut, "warning") || !strings.Contains(out, "Cash") {
		t.Fatalf("a raw read is quiet and renders: %d %q %q", code, out, errOut)
	}

	_, _, code = orRun(t, "raw", "DELETE", "accounts/list.json", "--api", srv.URL)

	if code != 2 {
		t.Fatalf("DELETE is not an upstream method: %d", code)
	}

	_, _, code = orRun(t, "raw", "GET", "tokens/list.json", "--api", srv.URL)

	if code != 2 {
		t.Fatalf("refused class: %d", code)
	}
}
