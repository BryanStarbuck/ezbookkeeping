package app

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/client"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

// WriteFlow runs the machine plane's write protocol for a write verb (cli.mdx §11):
//
//  1. call the route with dry_run: true and show the preview;
//  2. without --write, stop there — the preview IS the answer (exit 0);
//  3. with --write, echo the confirm_token with dry_run: false;
//  4. if the books moved in between (conflict), show the new preview and require --yes.
//
// body is the route's own arguments; this adds dry_run, confirm_token and max_changes.
func (c *Ctx) WriteFlow(method, path string, body map[string]any, view render.View) error {
	if body == nil {
		body = map[string]any{}
	}

	maxChanges, err := c.Int("max-changes", 0)

	if err != nil {
		return err
	}

	if maxChanges > 0 {
		body["max_changes"] = maxChanges
	}

	body["dry_run"] = true
	env, err := c.Call(method, path, nil, body)

	if err != nil {
		return c.explainCeiling(err)
	}

	var preview struct {
		Changes      map[string]int `json:"changes"`
		Warnings     []string       `json:"warnings"`
		ConfirmToken string         `json:"confirm_token"`
	}

	_ = env.Decode(&preview)

	if !c.Bool("write") {
		c.Info("DRY RUN — nothing was changed. %s. Re-run with --write to apply.", describeChanges(preview.Changes))

		for _, w := range preview.Warnings {
			c.Warnf("%s", w)
		}

		return c.Emit(env, view)
	}

	c.Info("target %s — applying: %s", c.BaseURL(), describeChanges(preview.Changes))

	for _, w := range preview.Warnings {
		c.Warnf("%s", w)
	}

	body["dry_run"] = false
	body["confirm_token"] = preview.ConfirmToken
	env2, err := c.Call(method, path, nil, body)

	if err != nil {
		ee, ok := err.(*ExitError)

		if ok && ee.APICode == "conflict" && !c.Bool("yes") {
			c.Info("the books changed since the preview; showing the new preview. Re-run with --write --yes to apply it.")

			body["dry_run"] = true
			delete(body, "confirm_token")

			if env3, err3 := c.Call(method, path, nil, body); err3 == nil {
				_ = c.Emit(env3, view)
			}

			return &ExitError{Code: exitcode.Conflict, Msg: ee.Msg, Hint: "review the new preview, then re-run with --write --yes"}
		}

		if ok && ee.APICode == "conflict" && c.Bool("yes") {
			body["dry_run"] = true
			delete(body, "confirm_token")
			env3, err3 := c.Call(method, path, nil, body)

			if err3 != nil {
				return err3
			}

			_ = env3.Decode(&preview)
			body["dry_run"] = false
			body["confirm_token"] = preview.ConfirmToken
			env2, err = c.Call(method, path, nil, body)

			if err != nil {
				return err
			}
		} else {
			return c.explainCeiling(err)
		}
	}

	return c.Emit(env2, view)
}

func (c *Ctx) explainCeiling(err error) error {
	ee, ok := err.(*ExitError)

	if !ok || ee.APICode != "conflict" {
		return err
	}

	if d, ok := ee.Details.(map[string]any); ok {
		if n, ok := render.ToInt64(d["would_change"]); ok {
			ee.Hint = fmt.Sprintf("%d rows would change; raise --max-changes (and add --yes) deliberately, or narrow the selection", n)
		}
	}

	return ee
}

func describeChanges(changes map[string]int) string {
	if len(changes) == 0 {
		return "no changes"
	}

	s := ""

	for _, k := range []string{"create", "update", "delete", "link", "unchanged", "skip"} {
		if n, ok := changes[k]; ok {
			if s != "" {
				s += ", "
			}

			s += fmt.Sprintf("%d %s", n, k)
		}
	}

	for k, n := range changes {
		switch k {
		case "create", "update", "delete", "link", "unchanged", "skip":
			continue
		}

		if s != "" {
			s += ", "
		}

		s += fmt.Sprintf("%d %s", n, k)
	}

	return s
}

// Query builds url.Values from alternating key/value pairs, skipping empty values
func Query(kv ...string) url.Values {
	q := url.Values{}

	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			q.Set(kv[i], kv[i+1])
		}
	}

	return q
}

// AddList appends a list flag to a query as a comma-separated value
func AddList(q url.Values, key string, values []string) {
	if len(values) > 0 {
		s := ""

		for i, v := range values {
			if i > 0 {
				s += ","
			}

			s += v
		}

		q.Set(key, s)
	}
}

// DecodeInto is a helper for verbs that post-process an envelope
func DecodeInto(env *client.Envelope, out any) error {
	if env == nil {
		return nil
	}

	return json.Unmarshal(env.Data, out)
}
