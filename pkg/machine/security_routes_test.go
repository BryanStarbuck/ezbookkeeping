package machine

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// security_routes_test.go — every route the plane mounts, not just /ping, refuses a caller that
// lacks the key, presents a wrong key, looks like a browser, arrives from off-box, or carries a
// rebinding Host (apis.mdx §5.6–§5.7, §19). It walks Routes() through the real Mount(), so a route
// registered outside the gated group — the one way a future edit could open a door — fails here.

// jrParamPath replaces every :param in a gin path with a harmless literal
func jrParamPath(p string) string {
	parts := strings.Split(p, "/")

	for i, s := range parts {
		if strings.HasPrefix(s, ":") || strings.HasPrefix(s, "*") {
			parts[i] = "1"
		}
	}

	return strings.Join(parts, "/")
}

func jrMountedEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	Mount(r, jrGateConfig())

	return r
}

func TestSecurityEveryRouteRequiresTheKey(t *testing.T) {
	jrArm(t, true, true)
	r := jrMountedEngine(t)
	want := `{"error":{"code":"unauthorized"},"ok":false}`

	if len(Routes()) == 0 {
		t.Fatal("Routes() is empty")
	}

	for i := range Routes() {
		rd := &Routes()[i]
		path := BasePath + jrParamPath(rd.Path)

		cases := map[string]jrReq{
			"missing key":     {method: rd.Method, path: path, noKey: true},
			"wrong key":       {method: rd.Method, path: path, key: strings.Repeat("e", 64)},
			"truncated key":   {method: rd.Method, path: path, key: jrGateKey[:32]},
			"empty key":       {method: rd.Method, path: path, key: " "},
			"key in query":    {method: rd.Method, path: path + "?api_key=" + jrGateKey, noKey: true},
			"bearer instead":  {method: rd.Method, path: path, noKey: true, headers: map[string]string{"Authorization": "Bearer " + jrGateKey}},
			"cookie instead":  {method: rd.Method, path: path, noKey: true, headers: map[string]string{"Cookie": HeaderAPIKey + "=" + jrGateKey}},
		}

		for name, q := range cases {
			w := jrDo(r, q)

			if w.Code != http.StatusUnauthorized || strings.TrimSpace(w.Body.String()) != want {
				t.Errorf("%s %s: %s answered %d %s, want 401 %s", rd.Method, rd.Path, name, w.Code, strings.TrimSpace(w.Body.String()), want)
			}

			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("%s %s: %s Cache-Control %q, want no-store", rd.Method, rd.Path, name, cc)
			}
		}
	}
}

func TestSecurityEveryRouteIs404OffBoxOrFromABrowser(t *testing.T) {
	jrArm(t, true, true)
	r := jrMountedEngine(t)

	for i := range Routes() {
		rd := &Routes()[i]
		path := BasePath + jrParamPath(rd.Path)

		cases := map[string]jrReq{
			"lan peer":            {method: rd.Method, path: path, remote: "10.0.0.7:4000"},
			"public peer":         {method: rd.Method, path: path, remote: "203.0.113.9:4000"},
			"forwarded-for":       {method: rd.Method, path: path, remote: "10.0.0.7:4000", headers: map[string]string{"X-Forwarded-For": "127.0.0.1"}},
			"origin":              {method: rd.Method, path: path, headers: map[string]string{"Origin": "https://evil.example"}},
			"same-origin fetch":   {method: rd.Method, path: path, headers: map[string]string{"Sec-Fetch-Site": "same-origin"}},
			"fetch mode":          {method: rd.Method, path: path, headers: map[string]string{"Sec-Fetch-Mode": "cors"}},
			"rebinding host":      {method: rd.Method, path: path, host: "evil.example:8080"},
			"rebinding host port": {method: rd.Method, path: path, host: "127.0.0.1:9999"},
		}

		for name, q := range cases {
			w := jrDo(r, q)

			if w.Code != http.StatusNotFound || !jrIsBare404(w) {
				t.Errorf("%s %s: %s answered %d %s, want the bare 404", rd.Method, rd.Path, name, w.Code, strings.TrimSpace(w.Body.String()))
			}
		}
	}
}

func TestSecurityMountRegistersNothingOutsideThePlane(t *testing.T) {
	jrArm(t, false, false)
	r := jrMountedEngine(t)

	for _, ri := range r.Routes() {
		if strings.HasPrefix(ri.Path, BasePath+"/") {
			continue
		}

		// the one keyless door is the loopback-only, write-only browser ingest route (error_err.mdx §9)
		if ri.Path == "/error-report" && ri.Method == http.MethodPost {
			continue
		}

		t.Errorf("Mount registered %s %s outside %s", ri.Method, ri.Path, BasePath)
	}

	// an unknown path under the plane sits behind the same gates: without the key it is the
	// constant 401 (route names are not an oracle), with the key it is the plane's own 404 envelope,
	// and from off-box it is the bare 404 — never gin's default page
	w := jrDo(r, jrReq{path: BasePath + "/no/such/route", noKey: true})

	if w.Code != http.StatusUnauthorized {
		t.Errorf("unknown plane path without key answered %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}

	w = jrDo(r, jrReq{path: BasePath + "/no/such/route"})

	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"code":"not_found"`) || !strings.Contains(w.Body.String(), "capabilities") {
		t.Errorf("unknown plane path with key answered %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}

	w = jrDo(r, jrReq{path: BasePath + "/no/such/route", remote: "10.0.0.7:4000"})

	if w.Code != http.StatusNotFound || !jrIsBare404(w) {
		t.Errorf("unknown plane path off-box answered %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
}
