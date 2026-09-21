package commands

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/credentials"
)

// orTestKey is a synthetic, well-formed key (never a real one)
const orTestKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// orTestEnv isolates a test from the real home directory: credentials, state dir, key overrides
func orTestEnv(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("EZBK_CREDENTIALS_FILE", filepath.Join(dir, "creds", "ezbookkeeping.json"))
	t.Setenv("EZBK_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("EZBK_API_KEY", "")
	t.Setenv("EZBK_API_KEY_FILE", "")
	t.Setenv("EZBK_API_URL", "")
	t.Setenv("EZBK_STATEMENTS_DIR", "")
	t.Setenv("TZ", "UTC")

	orCandCache = map[string][]orNamed{}

	return dir
}

// orTestPlane is a fake machine plane: /healthz.json, /machine/v1/ping and the given routes
func orTestPlane(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"2.0.1","commit":"abc1234","status":"ok"}`))

			return
		}

		if r.Header.Get("X-Ezbk-Api-Key") != orTestKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"unauthorized"}}`))

			return
		}

		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/machine/v1")

		if key == "GET /ping" {
			orWriteJSON(w, 200, map[string]any{"ok": true, "data": map[string]any{"pong": true, "tiers": map[string]bool{"read": true, "write": false, "admin": false}}})
			return
		}

		if h, ok := routes[key]; ok {
			h(w, r)
			return
		}

		orWriteJSON(w, 404, map[string]any{"ok": false, "error": map[string]any{"code": "not_found", "message": "no route " + key, "hint": "GET /machine/v1/capabilities"}})
	}))

	t.Cleanup(srv.Close)

	return srv
}

func orWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// orRun runs the CLI in-process and returns stdout, stderr and the exit code
func orRun(t *testing.T, argv ...string) (string, string, int) {
	t.Helper()

	var out, errb bytes.Buffer
	code := app.Main(argv, &out, &errb)

	return out.String(), errb.String(), code
}

func TestOrKeyInitShowRotateNeverPrintTheKey(t *testing.T) {
	dir := orTestEnv(t)

	out, errOut, code := orRun(t, "key", "init")

	if code != 0 {
		t.Fatalf("key init exit %d: %s", code, errOut)
	}

	creds, err := credentials.Read()

	if err != nil || !credentials.IsWellFormed(creds.APIKey) {
		t.Fatalf("key init did not mint a well-formed key: %v", err)
	}

	first := creds.APIKey

	if strings.Contains(out+errOut, first) {
		t.Fatal("key init printed the key itself")
	}

	if !strings.Contains(out, credentials.Fingerprint(first)) || !strings.Contains(out, "minted") {
		t.Fatalf("key init should print the fingerprint and say it minted: %q", out)
	}

	st, err := os.Stat(filepath.Join(dir, "creds", "ezbookkeeping.json"))

	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file must be 0600, got %v %v", st.Mode().Perm(), err)
	}

	// a second init keeps the key
	out, _, code = orRun(t, "key", "init")

	if code != 0 || !strings.Contains(out, "already present") {
		t.Fatalf("second key init: exit %d %q", code, out)
	}

	if c2, _ := credentials.Read(); c2.APIKey != first {
		t.Fatal("key init replaced an existing key")
	}

	out, _, code = orRun(t, "key", "show")

	if code != 0 || strings.Contains(out, first) || !strings.Contains(out, credentials.Fingerprint(first)) || !strings.Contains(out, "0600") {
		t.Fatalf("key show: exit %d %q", code, out)
	}

	out, _, code = orRun(t, "key", "show", "--format", "json")

	var facts map[string]any

	if code != 0 || json.Unmarshal([]byte(out), &facts) != nil || strings.Contains(out, first) {
		t.Fatalf("key show --format json must be one JSON object without the key: exit %d %q", code, out)
	}

	// rotate refuses without --yes, before touching anything
	_, errOut, code = orRun(t, "key", "rotate")

	if code != 2 || !strings.Contains(errOut, "--yes") {
		t.Fatalf("rotate without --yes: exit %d %q", code, errOut)
	}

	if c3, _ := credentials.Read(); c3.APIKey != first {
		t.Fatal("rotate without --yes changed the key")
	}

	t.Setenv("EZBK_PORT", "1") // nothing listens on port 1: "the server is not running"

	out, errOut, code = orRun(t, "key", "rotate", "--yes")

	if code != 0 {
		t.Fatalf("rotate --yes exit %d: %s", code, errOut)
	}

	c4, _ := credentials.Read()

	if c4.APIKey == first || !credentials.IsWellFormed(c4.APIKey) {
		t.Fatal("rotate --yes did not mint a new key")
	}

	if strings.Contains(out+errOut, c4.APIKey) || !strings.Contains(out, "restart") {
		t.Fatalf("rotate must print the new fingerprint and the restart reminder, never the key: %q", out)
	}
}

func TestOrKeyShowTooPermissive(t *testing.T) {
	dir := orTestEnv(t)

	if _, _, code := orRun(t, "key", "init"); code != 0 {
		t.Fatal("key init failed")
	}

	path := filepath.Join(dir, "creds", "ezbookkeeping.json")

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, _ := orRun(t, "key", "show")

	if !strings.Contains(out, "chmod 600") {
		t.Fatalf("key show should name the chmod fix: %q", out)
	}

	_, errOut, code := orRun(t, "key", "rotate", "--yes")

	if code != 2 || !strings.Contains(errOut, "chmod 600") {
		t.Fatalf("rotate over a 0644 file: exit %d %q", code, errOut)
	}
}

func TestOrBareNeverFailsWhenDown(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_PORT", "1")

	out, errOut, code := orRun(t)

	if code != 0 {
		t.Fatalf("bare ezbk must exit 0 with nothing running, got %d: %s", code, errOut)
	}

	for _, want := range []string{"ezBookkeeping — this machine", "DOWN", "ezbk up", "machine key", "none yet", "ezbk doctor"} {
		if !strings.Contains(out, want) {
			t.Errorf("orientation report lacks %q:\n%s", want, out)
		}
	}
}

func TestOrStatusAgainstFakePlane(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_API_KEY", orTestKey)

	srv := orTestPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /whoami": func(w http.ResponseWriter, r *http.Request) {
			orWriteJSON(w, 200, map[string]any{"ok": true, "data": map[string]any{"user": map[string]any{"username": "operator", "defaultCurrency": "USD"}, "timezone": "UTC"}})
		},
		"GET /health": func(w http.ResponseWriter, r *http.Request) {
			orWriteJSON(w, 200, map[string]any{"ok": true, "data": map[string]any{"userBound": true, "upstreamDoors": map[string]bool{"apiToken": false, "mcp": false}, "listenAddress": "127.0.0.1"}})
		},
	})

	out, errOut, code := orRun(t, "status", "--api", srv.URL)

	if code != 0 {
		t.Fatalf("status exit %d: %s", code, errOut)
	}

	if !strings.Contains(out, "UP") || !strings.Contains(out, `"operator"`) || !strings.Contains(out, "writes off") {
		t.Fatalf("status report:\n%s", out)
	}

	out, _, code = orRun(t, "status", "--api", srv.URL, "--format", "json")

	var p map[string]any

	if code != 0 || json.Unmarshal([]byte(out), &p) != nil || p["ping"] != "ok" {
		t.Fatalf("status json: exit %d %q", code, out)
	}
}

func TestOrStatusWrongKeyIsExit6(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_API_KEY", strings.Repeat("f", 64))

	srv := orTestPlane(t, nil)
	_, errOut, code := orRun(t, "status", "--api", srv.URL)

	if code != 6 || !strings.Contains(errOut, "ezbk stop && ezbk up") {
		t.Fatalf("a 401 from a reachable server is exit 6 naming the restart: %d %q", code, errOut)
	}
}

func TestOrUpRefusesForeignAPI(t *testing.T) {
	orTestEnv(t)

	_, errOut, code := orRun(t, "up", "--api", "http://127.0.0.1:9")

	if code != 2 || !strings.Contains(errOut, "cannot start") {
		t.Fatalf("up --api elsewhere: %d %q", code, errOut)
	}
}

func TestOrMissingTiers(t *testing.T) {
	have := map[string]bool{"read": true, "write": true, "admin": false}

	if m := orMissingTiers(true, false, have); len(m) != 0 {
		t.Fatalf("write is there: %v", m)
	}

	if m := orMissingTiers(true, true, have); len(m) != 1 || m[0] != "admin" {
		t.Fatalf("admin missing: %v", m)
	}

	if s := orUpFlags(true, true); s != "ezbk up --allow-write --allow-admin" {
		t.Fatal(s)
	}
}

func TestOrIniValue(t *testing.T) {
	ini := "[server]\nhttp_addr = 0.0.0.0\n; comment\n[security]\nsecret_key =\nenable_api_token = true\n[mcp]\nenable_mcp = false\n"

	if v, ok := orIniValueFrom(strings.NewReader(ini), "server", "http_addr"); !ok || v != "0.0.0.0" {
		t.Fatalf("http_addr: %q %v", v, ok)
	}

	if v, ok := orIniValueFrom(strings.NewReader(ini), "security", "enable_api_token"); !ok || !orTruthy(v) {
		t.Fatalf("enable_api_token: %q", v)
	}

	if v, ok := orIniValueFrom(strings.NewReader(ini), "security", "secret_key"); !ok || v != "" {
		t.Fatalf("secret_key should be present and empty: %q %v", v, ok)
	}

	if _, ok := orIniValueFrom(strings.NewReader(ini), "mcp", "absent"); ok {
		t.Fatal("absent key found")
	}
}

func TestOrVersionAtLeast(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"go1.27.1", "1.27.1", true},
		{"go1.27.2", "1.27.1", true},
		{"go1.28", "1.27.1", true},
		{"go1.27", "1.27.1", false},
		{"go1.26.9", "1.27", false},
		{"go1.27.1rc1", "1.27.1", true},
	}

	for _, tc := range cases {
		if got := orVersionAtLeast(tc.have, tc.want); got != tc.ok {
			t.Errorf("%s >= %s: got %v", tc.have, tc.want, got)
		}
	}
}

func TestOrIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1": true, "127.0.0.1:8080": true, "localhost": true, "::1": true, "[::1]": true, "0.0.0.0": false, "0.0.0.0:8080": false, "192.168.1.5": false, "": false} {
		if got := orIsLoopbackAddr(addr); got != want {
			t.Errorf("%q: got %v want %v", addr, got, want)
		}
	}
}

func TestOrGroupInt(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 14208: "14,208", 1234567: "1,234,567", -1000: "-1,000"} {
		if got := orGroupInt(n); got != want {
			t.Errorf("%d: %q", n, got)
		}
	}
}

func TestOrDoctorRequiredFailures(t *testing.T) {
	checks := []orCheck{{Name: "a", Required: true, Status: orOK}, {Name: "b", Required: true, Status: orFAIL}, {Name: "c", Status: orFAIL}, {Name: "d", Status: orWARN}}
	failed := orDoctorFailures(checks)

	if len(failed) != 1 || failed[0].Name != "b" {
		t.Fatalf("only required failures count: %v", failed)
	}
}

func TestOrDoctorCredentialsCheck(t *testing.T) {
	dir := orTestEnv(t)

	if ch := orCheckCredentials(); ch.Status != orFAIL || !strings.Contains(ch.Fix, "ezbk key init") {
		t.Fatalf("absent file: %+v", ch)
	}

	if _, _, code := orRun(t, "key", "init"); code != 0 {
		t.Fatal("key init failed")
	}

	if ch := orCheckCredentials(); ch.Status != orOK {
		t.Fatalf("minted file: %+v", ch)
	}

	_ = os.Chmod(filepath.Join(dir, "creds", "ezbookkeeping.json"), 0o640)

	if ch := orCheckCredentials(); ch.Status != orFAIL || !strings.Contains(ch.Fix, "chmod 600") {
		t.Fatalf("0640 file: %+v", ch)
	}
}

func TestOrDoorsWarnLoudly(t *testing.T) {
	p := &orProbe{UpstreamDoors: map[string]bool{"apiToken": true, "mcp": false}, DoorsFrom: "server"}
	ch := orCheckDoors(p)

	if ch.Status != orWARN || !strings.Contains(ch.Detail, "enable_api_token") {
		t.Fatalf("an open door must warn naming the .ini key: %+v", ch)
	}

	if line := orDoorsLine(p); !strings.Contains(line, "WARNING") {
		t.Fatalf("orientation line must warn: %q", line)
	}
}

func TestOrLogsReadsTheFiles(t *testing.T) {
	dir := orTestEnv(t)
	state := filepath.Join(dir, "state")
	_ = os.MkdirAll(state, 0o700)
	_ = os.WriteFile(filepath.Join(state, "server.log"), []byte("line one\nline two\nline three\n"), 0o600)

	out, _, code := orRun(t, "logs", "--only", "server", "--lines", "2")

	if code != 0 || !strings.Contains(out, "line two\nline three") || strings.Contains(out, "line one") {
		t.Fatalf("logs --lines 2: exit %d %q", code, out)
	}

	if _, _, code := orRun(t, "logs", "--only", "nope"); code != 2 {
		t.Fatalf("bad --only must be exit 2, got %d", code)
	}
}
