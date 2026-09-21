package commands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
)

func TestExSettingValue(t *testing.T) {
	cases := map[string]string{
		"true":     "true",
		"false":    "false",
		"12":       "12",
		"-3":       "-3",
		"1.5":      `"1.5"`,
		"hello":    `"hello"`,
		`{"a":1}`:  `{"a":1}`,
		`[1,2]`:    `[1,2]`,
		`{broken`:  `"{broken"`,
		"null":     "null",
		`"quoted"`: `"quoted"`,
	}

	for in, want := range cases {
		if got := string(exSettingValue(in)); got != want {
			t.Errorf("exSettingValue(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestExWeekday(t *testing.T) {
	for in, want := range map[string]int{"sun": 0, "Monday": 1, "tue": 2, "6": 6, "0": 0, "SAT": 6} {
		got, err := exWeekday(in)

		if err != nil || got != want {
			t.Errorf("exWeekday(%q) = %d, %v; want %d", in, got, err, want)
		}
	}

	for _, bad := range []string{"7", "-1", "su", "funday", ""} {
		if _, err := exWeekday(bad); err == nil {
			t.Errorf("exWeekday(%q) accepted", bad)
		}
	}
}

func TestExOnOff(t *testing.T) {
	for in, want := range map[string]bool{"on": true, "OFF": false, "yes": true, "0": false} {
		if got, err := exOnOff(in); err != nil || got != want {
			t.Errorf("exOnOff(%q) = %v, %v", in, got, err)
		}
	}

	if _, err := exOnOff("maybe"); err == nil {
		t.Error("exOnOff accepted maybe")
	}
}

func TestExParseKeyIds(t *testing.T) {
	got, err := exParseKeyIds([]string{"home/Bank/1111=3843979048535457792", "a=b=3843979048535457793"})

	if err != nil {
		t.Fatal(err)
	}

	if got["home/Bank/1111"] != "3843979048535457792" || got["a=b"] != "3843979048535457793" {
		t.Fatalf("got %v", got)
	}

	for _, bad := range [][]string{{"nokey"}, {"k="}, {"=3843979048535457792"}, {"k=Checking"}, {"k=3843979048535457792", "k=3843979048535457793"}} {
		if _, err := exParseKeyIds(bad); err == nil {
			t.Errorf("exParseKeyIds(%v) accepted", bad)
		}
	}
}

func TestExBatchFile(t *testing.T) {
	body, err := exBatchFile(json.RawMessage(`[{"op":"POST /tags","args":{"name":"x"}}]`))

	if err != nil || len(body["operations"].([]json.RawMessage)) != 1 {
		t.Fatalf("bare array: %v %v", body, err)
	}

	body, err = exBatchFile(json.RawMessage(`{"summary":"s","operations":[{"op":"POST /tags"},{"op":"POST /tags"}]}`))

	if err != nil || len(body["operations"].([]json.RawMessage)) != 2 || body["summary"] != "s" {
		t.Fatalf("object: %v %v", body, err)
	}

	if _, err := exBatchFile(json.RawMessage(`{"ops":[]}`)); err == nil {
		t.Fatal("accepted an object without operations")
	}
}

func TestExReadJSONArg(t *testing.T) {
	raw, err := exReadJSONArg(` {"a":1} `, nil)

	if err != nil || string(raw) != `{"a":1}` {
		t.Fatalf("inline: %s %v", raw, err)
	}

	raw, err = exReadJSONArg("-", strings.NewReader(`[1]`))

	if err != nil || string(raw) != `[1]` {
		t.Fatalf("stdin: %s %v", raw, err)
	}

	if _, err := exReadJSONArg("{nope", nil); err == nil {
		t.Fatal("accepted invalid JSON")
	}
}

func TestExFlatten(t *testing.T) {
	got := exFlatten("", map[string]any{"a": map[string]any{"b": 1.0}, "c": []any{"x"}, "d": nil})

	if got["a.b"] != "1" || got["c"] != `["x"]` || got["d"] != "" {
		t.Fatalf("got %v", got)
	}
}

func TestExVerbsRegistered(t *testing.T) {
	for _, name := range []string{
		"user show", "user edit", "user settings", "user settings set",
		"tag-groups add", "tag-groups edit", "tag-groups show",
		"templates add", "templates edit", "templates show", "templates hide", "templates unhide",
		"insights add", "insights edit", "insights show", "insights hide", "insights unhide",
		"accounts properties", "transactions earliest", "transactions latest", "transactions move-all",
		"batch", "batch ops", "capabilities",
		"statements map", "statements map set", "statements map infer", "statements rows",
		"statements runs", "statements run", "statements roots", "statements converters",
		"accounts move", "categories move", "tags move", "tag-groups move", "templates move", "insights move",
	} {
		v, used := app.Lookup(strings.Fields(name))

		if v == nil || v.Name != name || used != len(strings.Fields(name)) {
			t.Errorf("verb %q not registered (got %v)", name, v)
		}
	}
}

func TestExTemplateBodyAdd(t *testing.T) {
	v, _ := app.Lookup([]string{"templates", "add"})
	c := app.NewTestCtx(v, nil, map[string][]string{
		"name": {"Coffee"}, "account": {"Synth Card"}, "category": {"3843979076721180679"}, "amount": {"450"}, "tag": {"trip", "3843979106920169472"},
	}, map[string]bool{}, &strings.Builder{}, &strings.Builder{})

	if _, err := exTemplateBody(c, true); err == nil {
		t.Fatal("accepted a template without --type")
	}

	c = app.NewTestCtx(v, nil, map[string][]string{
		"name": {"Coffee"}, "type": {"expense"}, "account": {"Synth Card"}, "category": {"3843979076721180679"}, "amount": {"450"}, "tag": {"trip", "3843979106920169472"},
	}, map[string]bool{}, &strings.Builder{}, &strings.Builder{})

	body, err := exTemplateBody(c, true)

	if err != nil {
		t.Fatal(err)
	}

	if body["kind"] != "normal" || body["type"] != "expense" || body["account_name"] != "Synth Card" || body["category_id"] != "3843979076721180679" || body["amount"] != int64(450) {
		t.Fatalf("body %v", body)
	}

	if names, _ := body["tag_names"].([]string); len(names) != 1 || names[0] != "trip" {
		t.Fatalf("tag_names %v", body["tag_names"])
	}

	c = app.NewTestCtx(v, nil, map[string][]string{
		"name": {"Coffee"}, "type": {"expense"}, "account": {"Synth Card"}, "category": {"Food"}, "amount": {"4.50"},
	}, map[string]bool{}, &strings.Builder{}, &strings.Builder{})

	if _, err := exTemplateBody(c, true); err == nil {
		t.Fatal("accepted a decimal amount")
	}
}

func TestHoistLeadingFlags(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"--no-bringup", "status"}, []string{"status", "--no-bringup"}},
		{[]string{"--format", "json", "accounts", "list"}, []string{"accounts", "list", "--format", "json"}},
		{[]string{"--format=csv", "--quiet", "tags", "list", "--limit", "2"}, []string{"tags", "list", "--limit", "2", "--format=csv", "--quiet"}},
		{[]string{"--help"}, []string{"--help"}},
		{[]string{"accounts", "list"}, []string{"accounts", "list"}},
		{[]string{"--format", "json"}, []string{"--format", "json"}},
	}

	for _, c := range cases {
		got := app.HoistLeadingFlags(c.in)

		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("HoistLeadingFlags(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
