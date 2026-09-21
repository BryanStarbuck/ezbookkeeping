package cmd

import (
	"reflect"
	"testing"
)

func TestCommandWords(t *testing.T) {
	cases := map[string][]string{
		"ezbookkeeping server run":                                           {"server", "run"},
		"ezbookkeeping --conf-path conf/x.ini server run":                    {"server", "run"},
		"ezbookkeeping --conf-path=conf/x.ini --no-boot-log database update": {"database", "update"},
		"ezbookkeeping utility send-test-mail --to a@b":                      {"utility", "send-test-mail"},
		"ezbookkeeping": nil,
	}

	for in, want := range cases {
		argv := splitWords(in)

		if got := commandWords(argv); !reflect.DeepEqual(got, want) {
			t.Errorf("commandWords(%q) = %v want %v", in, got, want)
		}
	}
}

func splitWords(s string) []string {
	var out []string
	cur := ""

	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}

			continue
		}

		cur += string(r)
	}

	if cur != "" {
		out = append(out, cur)
	}

	return out
}
