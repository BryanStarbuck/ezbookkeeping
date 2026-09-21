package machine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// confirm.go is gate 6: plan tokens, fingerprints and ceilings (apis.mdx §9)

type confirmEntry struct {
	route       string
	uid         int64
	fingerprint string
	expires     time.Time
}

var confirmStore = struct {
	sync.Mutex
	m map[string]confirmEntry
}{m: map[string]confirmEntry{}}

// ChangeFingerprint is the sha256 of the canonical JSON of a change set
func ChangeFingerprint(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// issueConfirmToken returns an opaque 128-bit token bound to the route, the user and the change set
func issueConfirmToken(mc *Ctx, fingerprint string) (string, time.Time) {
	buf := make([]byte, 16)
	_, _ = io.ReadFull(rand.Reader, buf)
	token := "cf_" + hex.EncodeToString(buf)
	expires := time.Now().Add(ConfirmTTL)

	confirmStore.Lock()
	defer confirmStore.Unlock()

	now := time.Now()

	for k, v := range confirmStore.m {
		if now.After(v.expires) {
			delete(confirmStore.m, k)
		}
	}

	confirmStore.m[token] = confirmEntry{route: mc.Route.Method + " " + mc.Route.Path, uid: mc.Uid, fingerprint: fingerprint, expires: expires}

	return token, expires
}

// checkConfirmToken consumes a token if it matches; a mismatch reports the new counts
func checkConfirmToken(mc *Ctx, token, fingerprint string, changes any) error {
	if token == "" {
		return Invalid("call this route with dry_run: true (the default) first, then pass the confirm_token it returns", "a write with dry_run: false needs a confirm_token")
	}

	confirmStore.Lock()
	defer confirmStore.Unlock()

	entry, ok := confirmStore.m[token]

	if !ok || time.Now().After(entry.expires) {
		delete(confirmStore.m, token)
		return Conflict("re-run the dry run and use the new confirm_token (tokens last 10 minutes and do not survive a restart)", "the confirm_token is unknown or expired").WithDetails(map[string]any{"changes": changes})
	}

	if entry.route != mc.Route.Method+" "+mc.Route.Path || entry.uid != mc.Uid {
		return Conflict("use the confirm_token returned by this route's own dry run", "the confirm_token was issued for a different route or user")
	}

	if entry.fingerprint != fingerprint {
		delete(confirmStore.m, token)
		return Conflict("the books changed since the preview; review the new preview and confirm again", "the change set no longer matches the confirmed preview").WithDetails(map[string]any{"changes": changes, "fingerprint": fingerprint})
	}

	delete(confirmStore.m, token)

	return nil
}

// WriteOpts are the write-protocol arguments every write route accepts; embed it in the request
type WriteOpts struct {
	DryRun         *bool  `json:"dry_run,omitempty"`
	ConfirmToken   string `json:"confirm_token,omitempty"`
	MaxChanges     int    `json:"max_changes,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// IsDryRun reports the effective dry_run (it defaults to true, R4)
func (o WriteOpts) IsDryRun() bool {
	return o.DryRun == nil || *o.DryRun
}

// Plan is the resolved change set of a write — the write handler's own first half (apis.mdx §9.2)
type Plan struct {
	// Changes counts by kind, e.g. {"create": 1} or {"update": 46, "unchanged": 3}
	Changes map[string]int `json:"changes"`
	// Count is what the ceiling is measured against (API transactions, not database rows)
	Count int `json:"-"`
	// Preview is the exact before/after the caller will be shown
	Preview any `json:"preview,omitempty"`
	// Fingerprinted is what the fingerprint covers; defaults to Preview
	Fingerprinted any `json:"-"`
	// Warnings the caller should read before confirming
	Warnings []string `json:"warnings,omitempty"`
	// State is carried from the resolve half to the apply half (not serialised)
	State any `json:"-"`
}

// WriteResult is what a dry run returns
type WriteResult struct {
	DryRun       bool           `json:"dry_run"`
	Changes      map[string]int `json:"changes"`
	Preview      any            `json:"preview,omitempty"`
	Warnings     []string       `json:"warnings,omitempty"`
	ConfirmToken string         `json:"confirm_token,omitempty"`
	ExpiresAt    string         `json:"expires_at,omitempty"`
	Fingerprint  string         `json:"fingerprint,omitempty"`
	Result       any            `json:"result,omitempty"`
	JournalId    int64          `json:"journal_id,omitempty"`
}

// RunWrite runs the write protocol: resolve the plan, enforce the ceiling, and either return the
// preview with a confirm token (dry run) or re-resolve, check the token against the fresh
// fingerprint, and apply. The same resolve function produces both halves, so a preview can never
// disagree with its apply (R4).
func RunWrite(mc *Ctx, opts WriteOpts, resolve func() (*Plan, error), apply func(p *Plan) (any, error)) (any, error) {
	plan, err := resolve()

	if err != nil {
		return nil, err
	}

	if plan.Changes == nil {
		plan.Changes = map[string]int{}
	}

	fpSource := plan.Fingerprinted

	if fpSource == nil {
		fpSource = plan.Preview
	}

	fingerprint := ChangeFingerprint(fpSource)
	maxChanges := opts.MaxChanges

	if maxChanges <= 0 {
		maxChanges = DefaultMaxChanges
	}

	if plan.Count > maxChanges {
		return nil, Conflict("raise max_changes deliberately, or narrow the selection", "%d changes would be made; the ceiling is %d", plan.Count, maxChanges).WithDetails(map[string]any{"would_change": plan.Count, "max_changes": maxChanges, "changes": plan.Changes})
	}

	if opts.IsDryRun() {
		token, expires := issueConfirmToken(mc, fingerprint)
		mc.SetMeta("dryRun", true)

		return &WriteResult{
			DryRun:       true,
			Changes:      plan.Changes,
			Preview:      plan.Preview,
			Warnings:     plan.Warnings,
			ConfirmToken: token,
			ExpiresAt:    expires.UTC().Format(time.RFC3339),
			Fingerprint:  fingerprint,
		}, nil
	}

	if err := mc.RequireWriteTier(); err != nil {
		return nil, err
	}

	if err := checkConfirmToken(mc, opts.ConfirmToken, fingerprint, plan.Changes); err != nil {
		return nil, err
	}

	result, err := apply(plan)

	if err != nil {
		return nil, err
	}

	mc.SetMeta("dryRun", false)
	auditWrite(mc, plan.Count, true)

	out := &WriteResult{DryRun: false, Changes: plan.Changes, Result: result, Fingerprint: fingerprint}

	if id, ok := mc.meta["journalId"].(int64); ok {
		out.JournalId = id
	}

	return out, nil
}
