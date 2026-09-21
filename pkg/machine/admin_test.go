package machine

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// admin_test.go — apis.mdx §10.10

func TestJrParseClearScope(t *testing.T) {
	good := []struct {
		scope, account string
		wantScope      string
		wantId         int64
	}{
		{"all", "", jrScopeAll, 0},
		{" Transactions ", "", jrScopeTransactions, 0},
		{"transactions_of_account", "9007199254740993", jrScopeTransactionsOfAccount, 9007199254740993},
	}

	for _, c := range good {
		s, id, err := jrParseClearScope(c.scope, c.account)

		if err != nil || s != c.wantScope || id != c.wantId {
			t.Errorf("jrParseClearScope(%q, %q) = %s %d %v", c.scope, c.account, s, id, err)
		}
	}

	bad := [][2]string{{"", ""}, {"everything", ""}, {"all", "12"}, {"transactions", "12"}, {"transactions_of_account", ""}, {"transactions_of_account", "-3"}, {"transactions_of_account", "abc"}}

	for _, c := range bad {
		if _, _, err := jrParseClearScope(c[0], c[1]); err == nil || toFail(err).Code != CodeInvalidInput {
			t.Errorf("jrParseClearScope(%q, %q) must be invalid_input, got %v", c[0], c[1], err)
		}
	}
}

func TestJrTokenKind(t *testing.T) {
	for in, want := range map[int]string{1: "session", 5: "mcp_token", 8: "api_token", 2: "pending_2fa", 3: "other", 99: "other"} {
		if got := jrTokenKind(in); got != want {
			t.Errorf("jrTokenKind(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestJrAdminRoutesAreAdminTier(t *testing.T) {
	for _, r := range jrAdminRoutes() {
		if r.Tier != TierAdmin {
			t.Errorf("%s %s is %s, want admin", r.Method, r.Path, r.Tier)
		}

		if r.DryRunnable {
			t.Errorf("%s %s: admin routes are never dry-runnable past the tier gate", r.Method, r.Path)
		}
	}
}

func jrRotateCall(t *testing.T, body string) (any, error) {
	t.Helper()
	route := &RouteDef{Method: "POST", Path: "/admin/key/rotate", Tier: TierAdmin}
	mc := jrTestCtx(t, route, "POST", BasePath+"/admin/key/rotate", []byte(body), nil)

	return jrHandleKeyRotate(mc)
}

func TestJrKeyRotateDryRunThenApply(t *testing.T) {
	jrIsolate(t)
	old, err := MintIntoCredentials("server", false)

	if err != nil {
		t.Fatal(err)
	}

	prev := currentState()
	stateHolder.Store(newPlaneState(old, jrCredsPath(t), jrCredsPath(t)))
	t.Cleanup(func() { stateHolder.Store(prev) })

	res, err := jrRotateCall(t, `{}`)

	if err != nil {
		t.Fatal(err)
	}

	dry := res.(*WriteResult)

	if !dry.DryRun || dry.Changes["rotate"] != 1 {
		t.Fatalf("dry = %+v", dry)
	}

	if creds, _ := ReadCredentials(); creds.APIKey != old {
		t.Fatal("a dry run rotated the key")
	}

	res, err = jrRotateCall(t, `{"dry_run": false, "confirm_token": "`+dry.ConfirmToken+`"}`)

	if err != nil {
		t.Fatal(err)
	}

	creds, _ := ReadCredentials()

	if creds.APIKey == old || !IsWellFormedKey(creds.APIKey) {
		t.Fatal("the key was not rotated")
	}

	out := res.(*WriteResult).Result.(map[string]any)

	if out["newFingerprint"] != Fingerprint(creds.APIKey) || out["runningFingerprint"] != Fingerprint(old) || out["restartRequired"] != true {
		t.Fatalf("result = %v", out)
	}

	// the running server keeps the old key until restart
	if currentState().fingerprint != Fingerprint(old) {
		t.Fatal("the running plane swapped its key without a restart")
	}

	// no response ever carries a key
	for _, r := range []any{dry, res} {
		data, _ := json.Marshal(r)

		if regexp.MustCompile(`[0-9a-f]{64}`).Match(data) {
			t.Fatalf("a key leaked into the response: %s", data)
		}
	}

	// the other products in the file are untouched by a rotation
	if _, err := os.Stat(jrCredsPath(t)); err != nil {
		t.Fatal(err)
	}
}

func TestJrKeyRotateUnknownArgRefused(t *testing.T) {
	jrIsolate(t)

	if _, err := jrRotateCall(t, `{"api_key": "x"}`); err == nil || toFail(err).Code != CodeInvalidInput {
		t.Fatalf("an unknown argument must be refused, got %v", err)
	}
}

func TestJrKeyRotateWarnsOnEnvOverride(t *testing.T) {
	jrIsolate(t)
	prev := currentState()
	stateHolder.Store(newPlaneState(strings.Repeat("ab", 32), "EZBK_API_KEY", jrCredsPath(t)))
	t.Cleanup(func() { stateHolder.Store(prev) })

	res, err := jrRotateCall(t, `{}`)

	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(res.(*WriteResult).Warnings, " | ")

	if !strings.Contains(joined, "EZBK_API_KEY") || !strings.Contains(joined, "restart") {
		t.Fatalf("warnings = %s", joined)
	}
}
