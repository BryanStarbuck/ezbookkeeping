// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// vectors_test.go runs pm/error_err.mdx §15.4: one JSON file of inputs and expected outputs that
// the Go and TypeScript libraries both run. Synthetic data only.

type vectorFile struct {
	Paths []struct {
		Name   string            `json:"name"`
		Env    map[string]string `json:"env"`
		Test   bool              `json:"test"`
		UID    int               `json:"uid"`
		Expect string            `json:"expect"`
	} `json:"paths"`
	Format []struct {
		Name   string `json:"name"`
		Record struct {
			TS    string `json:"ts"`
			Level string `json:"level"`
			App   string `json:"app"`
			Where string `json:"where"`
			Doing string `json:"doing"`
			Fold  *struct {
				Count         int    `json:"count"`
				WindowSeconds int    `json:"windowSeconds"`
				FirstAt       string `json:"firstAt"`
			} `json:"fold"`
			Error vectorError `json:"error"`
			Data  [][]string  `json:"data"`
			Stack []string    `json:"stack"`
		} `json:"record"`
		Expect string `json:"expect"`
	} `json:"format"`
	Caps struct {
		Message        int    `json:"message"`
		MessageHead    int    `json:"message_head"`
		MessageTail    int    `json:"message_tail"`
		CutMarker      string `json:"message_cut_marker"`
		Data           int    `json:"data"`
		Record         int    `json:"record"`
		CauseDepth     int    `json:"cause_depth"`
		JoinMembers    int    `json:"join_members"`
		StackFrames    int    `json:"stack_frames"`
		FoldKeyMessage int    `json:"fold_key_message"`
	} `json:"caps"`
	ControlChars []struct {
		Input  string `json:"input"`
		Expect string `json:"expect"`
	} `json:"control_chars"`
	Redact []struct {
		Name   string         `json:"name"`
		Data   map[string]any `json:"data"`
		Expect map[string]any `json:"expect"`
	} `json:"redact"`
	RedactURLs []struct {
		Input  string `json:"input"`
		Expect string `json:"expect"`
	} `json:"redact_urls"`
	StatementsRoot []struct {
		Root   string `json:"root"`
		Input  string `json:"input"`
		Expect string `json:"expect"`
	} `json:"statements_root"`
	FoldKeys []struct {
		Message string `json:"message"`
		Expect  string `json:"expect"`
	} `json:"fold_keys"`
	FoldKeyParts []string `json:"fold_key_parts"`
	FoldWindows  struct {
		DefaultSeconds          int `json:"default_seconds"`
		TransientNetworkSeconds int `json:"transient_network_seconds"`
		MaxKeys                 int `json:"max_keys"`
	} `json:"fold_windows"`
	TransientNetworkMarkers []string `json:"transient_network_markers"`
	Normalise               struct {
		Steps []struct {
			Name    string `json:"name"`
			Regex   string `json:"regex"`
			Replace string `json:"replace"`
		} `json:"steps"`
	} `json:"normalise"`
	ControlCharsRegex    string   `json:"control_chars_regex"`
	SecretKeyRegex       string   `json:"secret_key_regex"`
	LedgerKeyRegex       string   `json:"ledger_key_regex"`
	URLQueryRedactParams []string `json:"url_query_redact_params"`
}

type vectorError struct {
	Type    string        `json:"type"`
	Message string        `json:"message"`
	Codes   [][]string    `json:"codes"`
	Cause   []vectorError `json:"cause"`
}

func loadVectors(t *testing.T) *vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")

	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}

	var v vectorFile

	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}

	return &v
}

func kvs(pairs [][]string) []KV {
	var out []KV

	for _, p := range pairs {
		if len(p) == 2 {
			out = append(out, KV{Key: p[0], Value: p[1]})
		}
	}

	return out
}

func TestVectorsPaths(t *testing.T) {
	v := loadVectors(t)

	for _, c := range v.Paths {
		env := c.Env
		lookup := func(name string) (string, bool) {
			val, ok := env[name]
			return val, ok
		}
		got := ResolvePath(lookup, c.Test, c.UID)

		if got != c.Expect {
			t.Errorf("%s: got %q want %q", c.Name, got, c.Expect)
		}
	}
}

func TestVectorsFormat(t *testing.T) {
	v := loadVectors(t)

	for _, c := range v.Format {
		r := c.Record
		ts, err := time.Parse(time.RFC3339Nano, r.TS)

		if err != nil {
			t.Fatalf("%s: bad ts %q", c.Name, r.TS)
		}

		headline := Headline(r.Error.Type, r.Error.Message, kvs(r.Error.Codes))
		rec := &Record{TS: ts, Level: Level(r.Level), App: r.App, Where: r.Where, Doing: r.Doing, Data: kvs(r.Data), Stack: r.Stack}

		if r.Fold != nil {
			rec.Error = SummaryText(r.Fold.Count, time.Duration(r.Fold.WindowSeconds)*time.Second, r.Fold.FirstAt, headline)
		} else {
			rec.Error = headline
			var b strings.Builder

			for i, cause := range r.Error.Cause {
				if i >= CauseDepth {
					break
				}

				b.WriteString(" | cause: " + Headline(cause.Type, cause.Message, kvs(cause.Codes)))
			}

			rec.Cause = b.String()
		}

		got := strings.TrimSuffix(FormatRecord(rec), "\n")

		if got != c.Expect {
			t.Errorf("%s:\n got: %q\nwant: %q", c.Name, got, c.Expect)
		}
	}
}

func TestVectorsCaps(t *testing.T) {
	v := loadVectors(t)

	if v.Caps.Message != MessageCap || v.Caps.Data != DataCap || v.Caps.Record != RecordCap ||
		v.Caps.CauseDepth != CauseDepth || v.Caps.JoinMembers != JoinMembers || v.Caps.StackFrames != StackFrames ||
		v.Caps.FoldKeyMessage != FoldKeyMessageCap {
		t.Errorf("caps differ from the vectors: %+v", v.Caps)
	}

	long := strings.Repeat("a", 3000)
	cut := capMiddle(long, MessageCap)

	if !strings.HasPrefix(cut, strings.Repeat("a", v.Caps.MessageHead)+" …(") || !strings.HasSuffix(cut, ")… "+strings.Repeat("a", v.Caps.MessageTail)) {
		t.Errorf("message cap shape wrong: head=%d tail=%d marker=%q", v.Caps.MessageHead, v.Caps.MessageTail, v.Caps.CutMarker)
	}

	if v.FoldWindows.DefaultSeconds != int(FoldWindow/time.Second) || v.FoldWindows.TransientNetworkSeconds != int(TransientFoldWindow/time.Second) || v.FoldWindows.MaxKeys != FoldMaxKeys {
		t.Errorf("fold windows differ from the vectors: %+v", v.FoldWindows)
	}
}

func TestVectorsControlChars(t *testing.T) {
	v := loadVectors(t)
	// The vectors write the regex in JS syntax (\u2028); RE2 spells that \x{2028}.
	re := regexp.MustCompile(regexp.MustCompile(`\\u([0-9a-fA-F]{4})`).ReplaceAllString(v.ControlCharsRegex, `\x{$1}`))

	for _, c := range v.ControlChars {
		if got := stripControlChars(c.Input); got != c.Expect {
			t.Errorf("stripControlChars(%q) = %q want %q", c.Input, got, c.Expect)
		}

		if got := re.ReplaceAllString(c.Input, " "); got != c.Expect {
			t.Errorf("the vectors' own regex disagrees for %q: %q", c.Input, got)
		}
	}
}

func TestVectorsRedact(t *testing.T) {
	v := loadVectors(t)

	if secretKeyRe.String() != v.SecretKeyRegex {
		t.Errorf("secret regex %q != vectors %q", secretKeyRe.String(), v.SecretKeyRegex)
	}

	if ledgerKeyRe.String() != v.LedgerKeyRegex {
		t.Errorf("ledger regex %q != vectors %q", ledgerKeyRe.String(), v.LedgerKeyRegex)
	}

	for _, p := range v.URLQueryRedactParams {
		if !redactedQueryParams[p] {
			t.Errorf("query param %q is in the vectors but not redacted", p)
		}
	}

	if len(redactedQueryParams) != len(v.URLQueryRedactParams) {
		t.Errorf("redacted query params: %d in code, %d in vectors", len(redactedQueryParams), len(v.URLQueryRedactParams))
	}

	for _, c := range v.Redact {
		for key, in := range c.Data {
			got := RedactValue(key, in)
			want := c.Expect[key]

			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: %s: got %#v want %#v", c.Name, key, got, want)
			}
		}
	}

	for _, c := range v.RedactURLs {
		if got := RedactURLs(c.Input); got != c.Expect {
			t.Errorf("RedactURLs(%q) = %q want %q", c.Input, got, c.Expect)
		}
	}

	for _, c := range v.StatementsRoot {
		if got := RedactStatementsPath(c.Input, c.Root); got != c.Expect {
			t.Errorf("RedactStatementsPath(%q, root=%q) = %q want %q", c.Input, c.Root, got, c.Expect)
		}
	}
}

func TestVectorsFoldKeys(t *testing.T) {
	v := loadVectors(t)

	for _, c := range v.FoldKeys {
		if got := NormalizeMessage(c.Message); got != c.Expect {
			t.Errorf("NormalizeMessage(%q) = %q want %q", c.Message, got, c.Expect)
		}
	}

	// The vectors' own regex steps, applied in order, must agree with the library's.
	steps := v.Normalise.Steps
	want := []string{normUUID.String(), normPath.String(), normHex.String(), normDigits.String()}

	if len(steps) != len(want) {
		t.Fatalf("normalise steps: %d in vectors, %d in code", len(steps), len(want))
	}

	for i, s := range steps {
		if s.Regex != want[i] {
			t.Errorf("normalise step %s: vectors %q code %q", s.Name, s.Regex, want[i])
		}
	}

	if !reflect.DeepEqual(v.FoldKeyParts, []string{"app", "where_file", "doing", "error_type", "normalised_message"}) {
		t.Errorf("fold key parts changed: %v", v.FoldKeyParts)
	}
}

func TestVectorsTransientMarkers(t *testing.T) {
	v := loadVectors(t)

	if !reflect.DeepEqual(v.TransientNetworkMarkers, transientMarkers) {
		t.Errorf("transient markers: vectors %v code %v", v.TransientNetworkMarkers, transientMarkers)
	}

	for _, m := range v.TransientNetworkMarkers {
		if !isTransientNetwork(&testErr{msg: "request failed: " + m}) {
			t.Errorf("marker %q is not detected as transient", m)
		}
	}
}

type testErr struct {
	msg   string
	cause error
}

func (e *testErr) Error() string { return e.msg }
func (e *testErr) Unwrap() error { return e.cause }
