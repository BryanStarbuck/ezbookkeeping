package machine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// confirm_test.go — gate 6: dry run default, tokens, fingerprints, ceilings (apis.mdx §9.1–§9.3)

func jrConfirmCtx(writeTier bool) *Ctx {
	return &Ctx{
		Route:       &RouteDef{Method: "POST", Path: "/jrtest/confirm", Tier: TierWrite, DryRunnable: true},
		Uid:         7,
		User:        &models.User{Uid: 7, Username: "operator"},
		Client:      "ezbk-test/0.0.0",
		WriteTierOn: writeTier,
	}
}

// jrPlanOf returns a resolver whose change set is whatever *preview holds at call time
func jrPlanOf(preview *any, count *int) func() (*Plan, error) {
	return func() (*Plan, error) {
		return &Plan{Changes: map[string]int{"update": *count}, Count: *count, Preview: *preview}, nil
	}
}

func jrBool(b bool) *bool { return &b }

func TestConfirmDryRunIsTheDefault(t *testing.T) {
	jrIsolate(t)
	mc := jrConfirmCtx(true)
	var preview any = []string{"row 1: a → b"}
	count := 1
	applied := false

	res, err := RunWrite(mc, WriteOpts{}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		applied = true
		return nil, nil
	})

	if err != nil {
		t.Fatal(err)
	}

	wr := res.(*WriteResult)

	if !wr.DryRun || applied {
		t.Fatal("a write with no dry_run must not apply")
	}

	if !strings.HasPrefix(wr.ConfirmToken, "cf_") || len(wr.ConfirmToken) != 3+32 {
		t.Fatalf("token %q is not cf_ + 128 bits of hex", wr.ConfirmToken)
	}

	if !strings.HasPrefix(wr.Fingerprint, "sha256:") {
		t.Fatalf("fingerprint %q", wr.Fingerprint)
	}

	exp, err := time.Parse(time.RFC3339, wr.ExpiresAt)

	if err != nil || exp.Sub(time.Now()) > ConfirmTTL+time.Second || exp.Sub(time.Now()) < ConfirmTTL-time.Minute {
		t.Fatalf("expires_at %s is not ~10 minutes out", wr.ExpiresAt)
	}

	if mc.meta["dryRun"] != true {
		t.Error("meta.dryRun must be true on a dry run")
	}

	// explicit dry_run: true is the same
	res, _ = RunWrite(mc, WriteOpts{DryRun: jrBool(true)}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		applied = true
		return nil, nil
	})

	if !res.(*WriteResult).DryRun || applied {
		t.Fatal("dry_run: true applied")
	}
}

func TestConfirmApplyNeedsToken(t *testing.T) {
	jrIsolate(t)
	mc := jrConfirmCtx(true)
	var preview any = "x"
	count := 1

	_, err := RunWrite(mc, WriteOpts{DryRun: jrBool(false)}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		t.Fatal("applied without a token")
		return nil, nil
	})

	if code := jrFailCode(t, err); code != CodeInvalidInput {
		t.Fatalf("code = %s", code)
	}

	_, err = RunWrite(mc, WriteOpts{DryRun: jrBool(false), ConfirmToken: "cf_00000000000000000000000000000000"}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		t.Fatal("applied with an invented token")
		return nil, nil
	})

	if code := jrFailCode(t, err); code != CodeConflict {
		t.Fatalf("invented token: code = %s", code)
	}
}

func TestConfirmHappyPathAndSingleUse(t *testing.T) {
	jrIsolate(t)
	mc := jrConfirmCtx(true)
	var preview any = map[string]any{"id": "9007199254740993", "from": 1, "to": 2}
	count := 1

	dry, _ := RunWrite(mc, WriteOpts{}, jrPlanOf(&preview, &count), nil)
	token := dry.(*WriteResult).ConfirmToken
	applies := 0

	res, err := RunWrite(mc, WriteOpts{DryRun: jrBool(false), ConfirmToken: token}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		applies++
		return map[string]any{"done": true}, nil
	})

	if err != nil || applies != 1 {
		t.Fatalf("apply: %v (applies %d)", err, applies)
	}

	if res.(*WriteResult).DryRun {
		t.Fatal("applied result says dry_run")
	}

	// the token is consumed
	_, err = RunWrite(mc, WriteOpts{DryRun: jrBool(false), ConfirmToken: token}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		applies++
		return nil, nil
	})

	if code := jrFailCode(t, err); code != CodeConflict || applies != 1 {
		t.Fatalf("token reuse: code %s applies %d", code, applies)
	}

	// the audit line names the route and count, never the preview's values
	audit, _ := os.ReadFile(filepath.Join(StateDir(), "machine.audit"))

	if !strings.Contains(string(audit), `route="POST /jrtest/confirm"`) || !strings.Contains(string(audit), "changed=1") || strings.Contains(string(audit), "9007199254740993") {
		t.Fatalf("audit = %s", audit)
	}
}

func TestConfirmStaleFingerprintReportsNewCounts(t *testing.T) {
	jrIsolate(t)
	mc := jrConfirmCtx(true)
	var preview any = []string{"a"}
	count := 46

	dry, _ := RunWrite(mc, WriteOpts{}, jrPlanOf(&preview, &count), nil)
	token := dry.(*WriteResult).ConfirmToken

	// the world moved: the browser edited a row
	preview = []string{"a", "b"}
	count = 47

	_, err := RunWrite(mc, WriteOpts{DryRun: jrBool(false), ConfirmToken: token}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		t.Fatal("applied a stale plan")
		return nil, nil
	})

	var f *Fail

	if !errors.As(err, &f) || f.Code != CodeConflict {
		t.Fatalf("err = %v", err)
	}

	d := f.Details.(map[string]any)

	if d["changes"].(map[string]int)["update"] != 47 {
		t.Fatalf("the refusal must carry the NEW counts, details = %v", d)
	}
}

func TestConfirmExpiredToken(t *testing.T) {
	jrIsolate(t)
	mc := jrConfirmCtx(true)
	var preview any = "x"
	count := 1

	dry, _ := RunWrite(mc, WriteOpts{}, jrPlanOf(&preview, &count), nil)
	token := dry.(*WriteResult).ConfirmToken

	confirmStore.Lock()
	e := confirmStore.m[token]
	e.expires = time.Now().Add(-time.Second)
	confirmStore.m[token] = e
	confirmStore.Unlock()

	_, err := RunWrite(mc, WriteOpts{DryRun: jrBool(false), ConfirmToken: token}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		t.Fatal("applied with an expired token")
		return nil, nil
	})

	if code := jrFailCode(t, err); code != CodeConflict {
		t.Fatalf("code = %s", code)
	}
}

func TestConfirmTokenBoundToRouteAndUser(t *testing.T) {
	jrIsolate(t)
	var preview any = "x"
	count := 1
	mc := jrConfirmCtx(true)
	dry, _ := RunWrite(mc, WriteOpts{}, jrPlanOf(&preview, &count), nil)
	token := dry.(*WriteResult).ConfirmToken

	other := jrConfirmCtx(true)
	other.Route = &RouteDef{Method: "POST", Path: "/jrtest/other"}

	if _, err := RunWrite(other, WriteOpts{DryRun: jrBool(false), ConfirmToken: token}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		t.Fatal("a token crossed routes")
		return nil, nil
	}); jrFailCode(t, err) != CodeConflict {
		t.Fatal("cross-route token not refused")
	}

	otherUser := jrConfirmCtx(true)
	otherUser.Uid = 8

	if _, err := RunWrite(otherUser, WriteOpts{DryRun: jrBool(false), ConfirmToken: token}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		t.Fatal("a token crossed users")
		return nil, nil
	}); jrFailCode(t, err) != CodeConflict {
		t.Fatal("cross-user token not refused")
	}
}

func TestConfirmCeilingReportsRealCount(t *testing.T) {
	jrIsolate(t)
	mc := jrConfirmCtx(true)
	var preview any = "x"
	count := DefaultMaxChanges + 1

	_, err := RunWrite(mc, WriteOpts{}, jrPlanOf(&preview, &count), nil)

	var f *Fail

	if !errors.As(err, &f) || f.Code != CodeConflict {
		t.Fatalf("err = %v", err)
	}

	d := f.Details.(map[string]any)

	if d["would_change"] != DefaultMaxChanges+1 || d["max_changes"] != DefaultMaxChanges {
		t.Fatalf("details = %v", d)
	}

	// raised deliberately, it passes
	if _, err := RunWrite(mc, WriteOpts{MaxChanges: count}, jrPlanOf(&preview, &count), nil); err != nil {
		t.Fatalf("raised ceiling: %v", err)
	}

	count = 5

	if _, err := RunWrite(mc, WriteOpts{MaxChanges: 4}, jrPlanOf(&preview, &count), nil); jrFailCode(t, err) != CodeConflict {
		t.Fatal("a lowered ceiling must refuse too")
	}
}

func TestConfirmApplyNeedsWriteTier(t *testing.T) {
	jrIsolate(t)
	var preview any = "x"
	count := 1
	mc := jrConfirmCtx(false)

	dry, err := RunWrite(mc, WriteOpts{}, jrPlanOf(&preview, &count), nil)

	if err != nil || !dry.(*WriteResult).DryRun {
		t.Fatal("a dry run works with the write tier off")
	}

	_, err = RunWrite(mc, WriteOpts{DryRun: jrBool(false), ConfirmToken: dry.(*WriteResult).ConfirmToken}, jrPlanOf(&preview, &count), func(p *Plan) (any, error) {
		t.Fatal("applied with the write tier off")
		return nil, nil
	})

	if code := jrFailCode(t, err); code != CodeWriteDisabled {
		t.Fatalf("code = %s", code)
	}
}

func TestConfirmFingerprintIsCanonical(t *testing.T) {
	a := ChangeFingerprint(map[string]any{"b": 1, "a": 2})
	b := ChangeFingerprint(map[string]any{"a": 2, "b": 1})

	if a != b {
		t.Fatal("map order must not change the fingerprint")
	}

	if a == ChangeFingerprint(map[string]any{"a": 2, "b": 2}) {
		t.Fatal("different change sets share a fingerprint")
	}

	// Fingerprinted overrides Preview
	mc := jrConfirmCtx(true)
	res, _ := RunWrite(mc, WriteOpts{}, func() (*Plan, error) {
		return &Plan{Changes: map[string]int{"x": 1}, Count: 1, Preview: "shown", Fingerprinted: "hashed"}, nil
	}, nil)

	if res.(*WriteResult).Fingerprint != ChangeFingerprint("hashed") {
		t.Fatal("Fingerprinted must be what the fingerprint covers")
	}

	if data, _ := json.Marshal(res); strings.Contains(string(data), "hashed") {
		t.Fatal("Fingerprinted leaked into the response")
	}
}
