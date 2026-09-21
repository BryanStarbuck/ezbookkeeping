package machine

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/gin-gonic/gin"
)

// passthrough_test.go — apis.mdx §16, §21 "Passthrough"

func TestJrPassthroughClasses(t *testing.T) {
	cases := []struct {
		method, path, class string
	}{
		{"GET", "/accounts/list.json", jrClassRead},
		{"GET", "/data/export.csv", jrClassRead},
		{"POST", "/accounts/add.json", jrClassWrite},
		{"POST", "/transactions/batch_update/category.json", jrClassWrite},
		{"POST", "/users/settings/cloud/disable.json", jrClassWrite},
		{"POST", "/accounts/delete.json", jrClassAdmin},
		{"POST", "/accounts/sub_account/delete.json", jrClassAdmin},
		{"POST", "/transactions/batch_delete.json", jrClassAdmin},
		{"POST", "/data/clear/all.json", jrClassAdmin},
		{"POST", "/data/clear/transactions/by_account.json", jrClassAdmin},
		{"POST", "/transaction/pictures/remove_unused.json", jrClassAdmin},
		{"POST", "/exchange_rates/user_custom/delete.json", jrClassAdmin},
		{"GET", "/tokens/list.json", jrClassRefused},
		{"POST", "/tokens/generate/api.json", jrClassRefused},
		{"POST", "/tokens/generate/mcp.json", jrClassRefused},
		{"GET", "/users/2fa/status.json", jrClassRefused},
		{"POST", "/users/external_auth/unlink.json", jrClassRefused},
		{"POST", "/users/avatar/update.json", jrClassRefused},
		{"POST", "/users/verify_email/resend.json", jrClassRefused},
		{"POST", "/llm/transactions/recognize_text.json", jrClassRefused},
	}

	for _, c := range cases {
		if got := jrPassthroughClass(c.method, c.path); got != c.class {
			t.Errorf("%s %s → %s, want %s", c.method, c.path, got, c.class)
		}
	}
}

// the table mirrors cmd/webserver.go: every upstream /api/v1 registration is either in the table or
// in a refused class, and nothing in the table is invented
func TestJrPassthroughTableMirrorsWebserver(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "webserver.go"))

	if err != nil {
		t.Skipf("cmd/webserver.go not readable: %v", err)
	}

	re := regexp.MustCompile(`apiV1Route\.(GET|POST)\("([^"]+)"`)
	matches := re.FindAllStringSubmatch(string(src), -1)

	if len(matches) < 50 {
		t.Fatalf("found only %d /api/v1 registrations; the pattern no longer matches webserver.go", len(matches))
	}

	upstream := map[string]bool{}

	for _, m := range matches {
		key := m[1] + " " + m[2]
		upstream[key] = true

		if jrPassthroughClass(m[1], m[2]) == jrClassRefused {
			if _, ok := jrLookupUpstream(m[1], m[2]); ok {
				t.Errorf("%s is refused but still in the table", key)
			}

			continue
		}

		if _, ok := jrLookupUpstream(m[1], m[2]); !ok {
			t.Errorf("upstream registers %s but the passthrough table does not", key)
		}
	}

	for key := range jrUpstreamTable() {
		if !upstream[key] {
			t.Errorf("the passthrough table has %s, which upstream does not register", key)
		}
	}
}

func jrPassthroughCall(t *testing.T, method, path, client string, body []byte) (any, error) {
	t.Helper()
	route := &RouteDef{Method: method, Path: "/api/*path", NoIntegerize: true}
	mc := jrTestCtx(t, route, method, BasePath+"/api"+path, body, gin.Params{{Key: "path", Value: path}})
	mc.Client = client

	return jrHandlePassthrough(mc)
}

func TestJrPassthroughRefusals(t *testing.T) {
	jrIsolate(t)
	prev := currentState()
	stateHolder.Store(nil) // admin off
	t.Cleanup(func() { stateHolder.Store(prev) })

	cases := []struct {
		method, path, client string
		body                 []byte
		code                 string
	}{
		{"GET", "/tokens/list.json", "ezbk/0.1", nil, CodeNotFound},
		{"POST", "/tokens/generate/api.json", "ezbk/0.1", []byte(`{}`), CodeNotFound},
		{"POST", "/llm/transactions/recognize_text.json", "ezbk/0.1", []byte(`{}`), CodeNotFound},
		{"GET", "/users/2fa/status.json", "ezbk/0.1", nil, CodeNotFound},
		{"GET", "/no/such/path.json", "ezbk/0.1", nil, CodeNotFound},
		{"GET", "/../../etc/passwd", "ezbk/0.1", nil, CodeNotFound},
		{"GET", "/accounts/add.json", "ezbk/0.1", nil, CodeInvalidInput},
		{"POST", "/accounts/delete.json", "ezbk/0.1", []byte(`{}`), CodeForbidden},
		{"POST", "/data/clear/all.json", "ezbk/0.1", []byte(`{}`), CodeForbidden},
		{"GET", "/accounts/list.json", "ezbookkeeping-mcp/0.1.0", nil, CodeForbidden},
		{"POST", "/users/profile/update.json", "ezbk/0.1", []byte(`{"password":"new-secret-1","oldPassword":"old-secret-1"}`), CodeForbidden},
		{"POST", "/users/profile/update.json", "ezbk/0.1", []byte(`{"email":"someone@example.invalid"}`), CodeForbidden},
	}

	for _, c := range cases {
		_, err := jrPassthroughCall(t, c.method, c.path, c.client, c.body)

		if err == nil {
			t.Errorf("%s %s: expected %s, got success", c.method, c.path, c.code)
			continue
		}

		if code := toFail(err).Code; code != c.code {
			t.Errorf("%s %s: code %s, want %s (%v)", c.method, c.path, code, c.code, err)
		}
	}
}

func TestJrPassthroughProfileBody(t *testing.T) {
	ok := [][]byte{[]byte(`{"nickname":"Op"}`), []byte(`{"password":""}`), []byte(`{"email":null,"defaultCurrency":"USD"}`)}

	for _, b := range ok {
		if err := jrCheckProfileBody(b); err != nil {
			t.Errorf("%s refused: %v", b, err)
		}
	}

	if err := jrCheckProfileBody([]byte(`[1]`)); err == nil {
		t.Error("a non-object body must be refused")
	}
}

func TestJrPassthroughCleanPath(t *testing.T) {
	for in, want := range map[string]string{"/accounts/list.json": "/accounts/list.json", "accounts/list.json": "/accounts/list.json", "/v1/accounts/list.json": "/accounts/list.json"} {
		got, err := jrCleanPassthroughPath(in)

		if err != nil || got != want {
			t.Errorf("clean(%q) = %q %v", in, got, err)
		}
	}

	for _, bad := range []string{"/../x", "/a//b", "/a\\b"} {
		if _, err := jrCleanPassthroughPath(bad); err == nil {
			t.Errorf("clean(%q) must be refused", bad)
		}
	}
}

func TestJrPassthroughRoutesDeclared(t *testing.T) {
	found := map[string]RouteDef{}

	for _, r := range jrPassthroughRoutes() {
		found[r.Method] = r
	}

	get, post := found["GET"], found["POST"]

	if get.Tier != TierRead || post.Tier != TierWrite || post.DryRunnable {
		t.Fatalf("passthrough tiers: GET %s, POST %s dryRunnable=%t", get.Tier, post.Tier, post.DryRunnable)
	}

	if !get.NoIntegerize || !post.NoIntegerize {
		t.Fatal("the passthrough returns upstream's result verbatim")
	}
}
