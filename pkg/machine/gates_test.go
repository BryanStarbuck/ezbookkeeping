package machine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// gates_test.go — the gate ladder, apis.mdx §5.6–§5.7 and §21 "Gate ladder"

const jrGateKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func jrGateConfig() *settings.Config {
	return &settings.Config{HttpAddr: "127.0.0.1", HttpPort: 8080, Protocol: settings.SCHEME_HTTP}
}

// jrArm installs a plane state for the test and restores the previous one afterwards
func jrArm(t *testing.T, allowWrite, allowAdmin bool) {
	t.Helper()
	prev := currentState()
	t.Setenv("EZBK_MACHINE_ALLOW_WRITE", map[bool]string{true: "1", false: ""}[allowWrite])
	t.Setenv("EZBK_MACHINE_ALLOW_ADMIN", map[bool]string{true: "1", false: ""}[allowAdmin])
	stateHolder.Store(newPlaneState(jrGateKey, "test", ""))
	t.Cleanup(func() { stateHolder.Store(prev) })
}

func jrGateEngine(t *testing.T, routes ...RouteDef) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := jrGateConfig()
	r := gin.New()
	g := r.Group(BasePath)
	g.Use(gateMiddleware(cfg))

	g.GET("/probe", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true, "reached": true}) })

	for i := range routes {
		rd := routes[i]

		if rd.Status == "" {
			rd.Status = StatusLive
		}

		g.Handle(rd.Method, rd.Path, wrap(&rd, cfg))
	}

	return r
}

type jrReq struct {
	method, path, remote, host string
	headers                    map[string]string
	noKey                      bool
	key                        string
}

func jrDo(r *gin.Engine, q jrReq) *httptest.ResponseRecorder {
	if q.method == "" {
		q.method = "GET"
	}

	if q.path == "" {
		q.path = BasePath + "/probe"
	}

	req := httptest.NewRequest(q.method, q.path, strings.NewReader("{}"))
	req.RemoteAddr = q.remote

	if req.RemoteAddr == "" {
		req.RemoteAddr = "127.0.0.1:53000"
	}

	req.Host = q.host

	if req.Host == "" {
		req.Host = "127.0.0.1:8080"
	}

	if !q.noKey {
		k := q.key

		if k == "" {
			k = jrGateKey
		}

		req.Header.Set(HeaderAPIKey, k)
	}

	for k, v := range q.headers {
		req.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

func TestGatesHappyPath(t *testing.T) {
	jrArm(t, false, false)
	r := jrGateEngine(t)

	for _, remote := range []string{"127.0.0.1:1", "[::1]:2", "[::ffff:127.0.0.1]:3"} {
		for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "LOCALHOST:8080"} {
			w := jrDo(r, jrReq{remote: remote, host: host})

			if w.Code != 200 {
				t.Errorf("remote %s host %s: %d %s", remote, host, w.Code, w.Body)
			}

			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Vary") != "Origin" {
				t.Errorf("missing no-store / Vary headers")
			}
		}
	}
}

func TestGatesNonLoopbackIs404EvenWithForwardedFor(t *testing.T) {
	jrArm(t, false, false)
	r := jrGateEngine(t)

	if err := r.SetTrustedProxies([]string{"0.0.0.0/0", "::/0"}); err != nil {
		t.Fatal(err)
	}

	cases := []jrReq{
		{remote: "192.168.1.20:5555"},
		{remote: "192.168.1.20:5555", headers: map[string]string{"X-Forwarded-For": "127.0.0.1"}},
		{remote: "192.168.1.20:5555", headers: map[string]string{"X-Real-IP": "127.0.0.1"}},
		{remote: "10.0.0.2:5555", headers: map[string]string{"X-Forwarded-For": "127.0.0.1, 127.0.0.1"}},
		{remote: "[2001:db8::1]:5555"},
	}

	for _, q := range cases {
		w := jrDo(r, q)

		if w.Code != 404 || !jrIsBare404(w) {
			t.Errorf("remote %s headers %v: %d %s", q.remote, q.headers, w.Code, w.Body)
		}
	}
}

func jrIsBare404(w *httptest.ResponseRecorder) bool {
	return strings.TrimSpace(w.Body.String()) == `{"error":{"code":"not_found"},"ok":false}`
}

func TestGatesBrowserSignalsAre404(t *testing.T) {
	jrArm(t, false, false)
	r := jrGateEngine(t)

	for _, h := range []map[string]string{
		{"Origin": "https://evil.example"},
		{"Origin": "http://127.0.0.1:8080"},
		{"Origin": "null"},
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-origin"},
		{"Sec-Fetch-Mode": "cors"},
	} {
		w := jrDo(r, jrReq{headers: h})

		if w.Code != 404 || !jrIsBare404(w) {
			t.Errorf("headers %v: %d %s", h, w.Code, w.Body)
		}
	}
}

func TestGatesRebindingHostIs404(t *testing.T) {
	jrArm(t, false, false)
	r := jrGateEngine(t)

	for _, host := range []string{"evil.example:8080", "evil.example", "127.0.0.1:9999", "127.0.0.1", "localhost.evil.example:8080", "0.0.0.0:8080", "192.168.1.20:8080"} {
		w := jrDo(r, jrReq{host: host})

		if w.Code != 404 {
			t.Errorf("host %s: %d", host, w.Code)
		}
	}
}

func TestGatesKeyBodyIsConstant(t *testing.T) {
	jrArm(t, false, false)
	r := jrGateEngine(t)

	missing := jrDo(r, jrReq{noKey: true})
	wrong := jrDo(r, jrReq{key: strings.Repeat("f", 64)})
	short := jrDo(r, jrReq{key: "0123"})
	long := jrDo(r, jrReq{key: jrGateKey + jrGateKey})
	prefix := jrDo(r, jrReq{key: jrGateKey[:63]})

	want := `{"error":{"code":"unauthorized"},"ok":false}`

	for name, w := range map[string]*httptest.ResponseRecorder{"missing": missing, "wrong": wrong, "short": short, "long": long, "prefix": prefix} {
		if w.Code != 401 || strings.TrimSpace(w.Body.String()) != want {
			t.Errorf("%s key: %d %s", name, w.Code, w.Body)
		}
	}

	if missing.Body.String() != wrong.Body.String() {
		t.Error("missing and wrong key bodies differ")
	}

	// the key is never accepted from the query string or Authorization
	req := httptest.NewRequest("GET", BasePath+"/probe?api_key="+jrGateKey, nil)
	req.RemoteAddr, req.Host = "127.0.0.1:1", "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+jrGateKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("query/Authorization key accepted: %d", w.Code)
	}
}

func TestGatesUnarmedIs404(t *testing.T) {
	prev := currentState()
	stateHolder.Store(nil)
	t.Cleanup(func() { stateHolder.Store(prev) })
	r := jrGateEngine(t)

	if w := jrDo(r, jrReq{}); w.Code != 404 || !jrIsBare404(w) {
		t.Fatalf("unarmed plane answered %d %s", w.Code, w.Body)
	}
}

func TestGatesKeyMatches(t *testing.T) {
	st := newPlaneState(jrGateKey, "test", "")

	if !keyMatches(jrGateKey, st.keyDigest) {
		t.Fatal("the right key must match")
	}

	for _, k := range []string{"", jrGateKey[:63], jrGateKey + "0", strings.ToUpper(jrGateKey)} {
		if keyMatches(k, st.keyDigest) {
			t.Errorf("%q must not match", k)
		}
	}
}

// gate 4 — tiers, through the real wrap()

func jrTierRoutes(reached *[]string) []RouteDef {
	h := func(name string) HandlerFunc {
		return func(mc *Ctx) (any, error) {
			*reached = append(*reached, name)
			return map[string]any{"ok": name}, nil
		}
	}

	return []RouteDef{
		{Method: "POST", Path: "/w", Tier: TierWrite, NoUser: true, Handler: h("w")},
		{Method: "POST", Path: "/wd", Tier: TierWrite, NoUser: true, DryRunnable: true, Handler: h("wd")},
		{Method: "POST", Path: "/a", Tier: TierAdmin, NoUser: true, Handler: h("a")},
		{Method: "GET", Path: "/r", Tier: TierRead, NoUser: true, Handler: h("r")},
	}
}

func jrErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Ok    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
		} `json:"error"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not an envelope: %s", w.Body)
	}

	if body.Ok {
		return ""
	}

	if body.Error.Code != CodeUnauthorized && body.Error.Code != CodeNotFound && body.Error.Hint == "" {
		t.Errorf("error %s has no hint (R6)", body.Error.Code)
	}

	return body.Error.Code
}

func TestGatesTierWriteOff(t *testing.T) {
	jrIsolate(t)
	jrArm(t, false, false)
	var reached []string
	r := jrGateEngine(t, jrTierRoutes(&reached)...)

	if w := jrDo(r, jrReq{method: "POST", path: BasePath + "/w"}); w.Code != http.StatusForbidden || jrErrCode(t, w) != CodeWriteDisabled {
		t.Errorf("write with tier off: %d %s", w.Code, w.Body)
	}

	// a dry-runnable write reaches its handler (the apply step checks the tier itself)
	if w := jrDo(r, jrReq{method: "POST", path: BasePath + "/wd"}); w.Code != 200 {
		t.Errorf("dry-runnable write: %d %s", w.Code, w.Body)
	}

	if w := jrDo(r, jrReq{method: "POST", path: BasePath + "/a"}); w.Code != http.StatusForbidden || jrErrCode(t, w) != CodeForbidden {
		t.Errorf("admin with tier off: %d %s", w.Code, w.Body)
	}

	if strings.Join(reached, ",") != "wd" {
		t.Errorf("handlers reached = %v", reached)
	}
}

func TestGatesTierAdmin(t *testing.T) {
	jrIsolate(t)
	var reached []string

	// write on, admin off → admin forbidden
	jrArm(t, true, false)
	r := jrGateEngine(t, jrTierRoutes(&reached)...)

	if w := jrDo(r, jrReq{method: "POST", path: BasePath + "/a"}); jrErrCode(t, w) != CodeForbidden {
		t.Errorf("admin with write only: %s", w.Body)
	}

	if w := jrDo(r, jrReq{method: "POST", path: BasePath + "/w"}); w.Code != 200 {
		t.Errorf("write with write on: %d %s", w.Code, w.Body)
	}

	// admin on, but the MCP asks → forbidden
	jrArm(t, true, true)
	r = jrGateEngine(t, jrTierRoutes(&reached)...)

	w := jrDo(r, jrReq{method: "POST", path: BasePath + "/a", headers: map[string]string{HeaderClient: "ezbookkeeping-mcp/0.1.0"}})

	if jrErrCode(t, w) != CodeForbidden {
		t.Errorf("admin from the MCP: %s", w.Body)
	}

	if w := jrDo(r, jrReq{method: "POST", path: BasePath + "/a", headers: map[string]string{HeaderClient: "ezbk/0.1.0"}}); w.Code != 200 {
		t.Errorf("admin from the CLI with admin on: %d %s", w.Code, w.Body)
	}

	if strings.Join(reached, ",") != "w,a" {
		t.Errorf("handlers reached = %v", reached)
	}
}

// wrap() integerizes amounts by default and leaves them verbatim for NoIntegerize (passthrough)
func TestGatesNoIntegerize(t *testing.T) {
	jrIsolate(t)
	jrArm(t, false, false)
	h := func(mc *Ctx) (any, error) {
		return map[string]any{"amount": "123456", "id": "9007199254740993", "items": []any{map[string]any{"balance": "-5"}}}, nil
	}
	r := jrGateEngine(t,
		RouteDef{Method: "GET", Path: "/int", Tier: TierRead, NoUser: true, Handler: h},
		RouteDef{Method: "GET", Path: "/raw", Tier: TierRead, NoUser: true, Handler: h, NoIntegerize: true},
	)

	w := jrDo(r, jrReq{path: BasePath + "/int"})

	if !strings.Contains(w.Body.String(), `"amount":123456`) || !strings.Contains(w.Body.String(), `"balance":-5`) || !strings.Contains(w.Body.String(), `"id":"9007199254740993"`) {
		t.Errorf("integerized body = %s", w.Body)
	}

	w = jrDo(r, jrReq{path: BasePath + "/raw"})

	if !strings.Contains(w.Body.String(), `"amount":"123456"`) || !strings.Contains(w.Body.String(), `"balance":"-5"`) {
		t.Errorf("verbatim body = %s", w.Body)
	}
}

// §8.3: /capabilities is projected from the same array the router is built from
func TestGatesCapabilityParity(t *testing.T) {
	jrIsolate(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := jrGateConfig()
	Mount(r, cfg)

	mounted := map[string]bool{}

	for _, ri := range r.Routes() {
		mounted[ri.Method+" "+ri.Path] = true
	}

	routes, _ := projectCapabilities(cfg)
	published := 0

	for _, cr := range routes {
		for _, m := range cr.Methods {
			published++

			if !mounted[m+" "+BasePath+cr.Path] {
				t.Errorf("%s %s is published but not mounted", m, cr.Path)
			}
		}
	}

	machineMounted := 0

	for k := range mounted {
		if strings.Contains(k, " "+BasePath+"/") {
			machineMounted++
		}
	}

	if machineMounted != published {
		t.Errorf("mounted %d machine routes, published %d", machineMounted, published)
	}

	jrArm(t, false, false)

	if w := jrDo(r, jrReq{path: BasePath + "/no-such-route"}); w.Code != 404 || jrErrCode(t, w) != CodeNotFound || jrIsBare404(w) {
		t.Errorf("an unknown route behind the gates must be the plane's own not_found envelope: %d %s", w.Code, w.Body)
	}

	if w := jrDo(r, jrReq{path: BasePath + "/no-such-route", noKey: true}); w.Code != 401 {
		t.Errorf("an unknown route without the key must be 401 (gates first): %d", w.Code)
	}
}
