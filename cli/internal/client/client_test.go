package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// upstream wraps /healthz.json as {"result":{"status":"ok",…},"success":true}
func TestHealthzReadsUpstreamEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"status":"ok","version":"1.2.3","commit":"abc"},"success":true}`))
	}))
	defer srv.Close()

	m, err := Healthz(srv.URL, 2*time.Second)

	if err != nil {
		t.Fatal(err)
	}

	if m["status"] != "ok" || m["version"] != "1.2.3" || m["commit"] != "abc" {
		t.Fatalf("got %v", m)
	}
}

func TestHealthzFlatStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	if m, err := Healthz(srv.URL, 2*time.Second); err != nil || m["status"] != "ok" {
		t.Fatalf("got %v %v", m, err)
	}
}
