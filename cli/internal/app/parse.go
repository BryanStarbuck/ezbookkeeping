package app

import (
	"fmt"
	"strings"
)

// Parsed is a verb invocation after argument parsing
type Parsed struct {
	Verb   *Verb
	Args   []string
	Values map[string][]string
	Bools  map[string]bool
}

// UsageError is a local refusal before any call: exit 2
type UsageError struct {
	Msg  string
	Hint string
}

func (e *UsageError) Error() string { return e.Msg }

func flagIndex(v *Verb) map[string]Flag {
	idx := map[string]Flag{}

	for _, f := range UniversalFlags {
		idx[f.Name] = f
	}

	if v != nil {
		for _, f := range v.Flags {
			idx[f.Name] = f
		}
	}

	return idx
}

// Parse resolves the verb from the leading words and parses flags anywhere after it. Positional
// first, flags anywhere; `--flag=value` and `--flag value` both work; `--` ends flag parsing.
func Parse(argv []string) (*Parsed, error) {
	var words []string

	for _, a := range argv {
		if strings.HasPrefix(a, "-") {
			break
		}

		words = append(words, a)
	}

	verb, used := Lookup(words)

	if verb == nil {
		if len(words) == 0 {
			return &Parsed{Values: map[string][]string{}, Bools: map[string]bool{}}, nil
		}

		return nil, &UsageError{Msg: fmt.Sprintf("unknown command %q", strings.Join(words, " ")), Hint: "ezbk help lists every command"}
	}

	return ParseFor(verb, argv[used:])
}

// HoistLeadingFlags moves universal flags typed before the verb (`ezbk --no-bringup status`)
// to after it, so they are parsed exactly as if typed at the end. --help/-h stay put.
func HoistLeadingFlags(argv []string) []string {
	var lead []string
	i := 0

	for i < len(argv) {
		a := argv[i]

		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" || a == "-h" || a == "--help" {
			break
		}

		name := strings.TrimLeft(a, "-")

		if strings.Contains(name, "=") {
			lead = append(lead, a)
			i++
			continue
		}

		takesValue := false

		for _, f := range UniversalFlags {
			if f.Name == name {
				takesValue = f.Value != ""
			}
		}

		lead = append(lead, a)
		i++

		if takesValue && i < len(argv) {
			lead = append(lead, argv[i])
			i++
		}
	}

	if len(lead) == 0 {
		return argv
	}

	out := append([]string{}, argv[i:]...)

	return append(out, lead...)
}

// ParseFor parses the flags and positionals that follow an already-resolved verb
func ParseFor(verb *Verb, rest []string) (*Parsed, error) {
	p := &Parsed{Verb: verb, Values: map[string][]string{}, Bools: map[string]bool{}}
	idx := flagIndex(verb)
	endFlags := false

	for i := 0; i < len(rest); i++ {
		a := rest[i]

		if endFlags || !strings.HasPrefix(a, "-") || a == "-" || isNegativeNumber(a) {
			p.Args = append(p.Args, a)
			continue
		}

		if a == "--" {
			endFlags = true
			continue
		}

		if a == "-h" {
			p.Bools["help"] = true
			continue
		}

		name := strings.TrimLeft(a, "-")
		value := ""
		hasValue := false

		if eq := strings.Index(name, "="); eq >= 0 {
			name, value, hasValue = name[:eq], name[eq+1:], true
		}

		f, ok := idx[name]

		if !ok {
			return nil, &UsageError{Msg: fmt.Sprintf("unknown flag --%s for `ezbk %s`", name, verb.Name), Hint: suggest(name, idx, verb)}
		}

		if f.Value == "" {
			if hasValue {
				p.Bools[name] = value == "" || value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
			} else {
				p.Bools[name] = true
			}

			continue
		}

		if !hasValue {
			if i+1 >= len(rest) {
				return nil, &UsageError{Msg: fmt.Sprintf("--%s needs a value (%s)", name, f.Value), Hint: "ezbk help " + verb.Name}
			}

			i++
			value = rest[i]
		}

		if !f.Repeat && len(p.Values[name]) > 0 {
			return nil, &UsageError{Msg: fmt.Sprintf("--%s was given more than once", name), Hint: "ezbk help " + verb.Name}
		}

		p.Values[name] = append(p.Values[name], value)
	}

	if p.Bools["help"] {
		return p, nil
	}

	if len(p.Args) < verb.MinArgs {
		return nil, &UsageError{Msg: fmt.Sprintf("`ezbk %s` needs %s", verb.Name, orDefault(verb.Args, "more arguments")), Hint: "ezbk help " + verb.Name}
	}

	if verb.MaxArgs >= 0 && len(p.Args) > verb.MaxArgs {
		extra := p.Args[verb.MaxArgs]
		hint := "ezbk help " + verb.Name

		return nil, &UsageError{Msg: fmt.Sprintf("unexpected argument %q for `ezbk %s`", extra, verb.Name), Hint: hint}
	}

	return p, nil
}

func isNegativeNumber(a string) bool {
	if len(a) < 2 || a[0] != '-' {
		return false
	}

	for _, r := range a[1:] {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}

	return true
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}

	return s
}

// suggest names the nearest declared flag by edit distance
func suggest(name string, idx map[string]Flag, v *Verb) string {
	best, bestD := "", 1<<30

	for k := range idx {
		if d := editDistance(name, k); d < bestD {
			best, bestD = k, d
		}
	}

	if best != "" && bestD <= 3 {
		return fmt.Sprintf("did you mean --%s?  (ezbk help %s)", best, v.Name)
	}

	return "ezbk help " + v.Name
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)

	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i

		for j := 1; j <= len(b); j++ {
			cost := 1

			if a[i-1] == b[j-1] {
				cost = 0
			}

			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}

		prev = cur
	}

	return prev[len(b)]
}
