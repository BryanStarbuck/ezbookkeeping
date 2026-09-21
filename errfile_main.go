package main

import "strings"

// commandName is the subcommand of argv ("server run", "database update") for the main() net
// (pm/error_err.mdx §7 G8), skipping the global flags and their values. Never the whole argv: a
// flag value could carry an address or a path.
func commandName(argv []string) string {
	var words []string

	for i := 1; i < len(argv) && len(words) < 2; i++ {
		a := argv[i]

		if strings.HasPrefix(a, "-") {
			if a == "--conf-path" || a == "-conf-path" {
				i++
			}

			continue
		}

		words = append(words, a)
	}

	return strings.Join(words, " ")
}
