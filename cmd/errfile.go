package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// EnvCanary switches the hidden canary subcommand on (pm/error_err.mdx §15.3)
const EnvCanary = "EZBK_ERROR_FILE_CANARY"

// errfileOptions builds the Install options for net N5 of pm/error_err.mdx §8: App is "server" for
// `server run` and "app-cli" for every other subcommand (§3.3). The binary has no signal handling
// of its own, so the library flushes on SIGINT/SIGTERM/SIGHUP.
func errfileOptions(config *settings.Config) errfile.Options {
	words := commandWords(os.Args)
	app := "app-cli"

	if len(words) > 0 && words[0] == "server" {
		app = "server"
	}

	return errfile.Options{
		App:           app,
		Command:       strings.Join(words, " "),
		HandleSignals: true,
		Development:   config != nil && config.Mode == settings.MODE_DEVELOPMENT,
	}
}

// commandWords returns the subcommand words of argv ("server run", "database update"), skipping
// the global flags and their values.
func commandWords(argv []string) []string {
	var words []string

	for i := 1; i < len(argv) && len(words) < 2; i++ {
		a := argv[i]

		if strings.HasPrefix(a, "-") {
			if !strings.Contains(a, "=") && (a == "--conf-path" || a == "-conf-path") {
				i++
			}

			continue
		}

		words = append(words, a)
	}

	return words
}

// errfileCanary is `ezbookkeeping utility __canary` (pm/error_err.mdx §15.3): one deliberate fault
// from the app-cli runtime, so the file can be checked end to end. Inert without the env var.
func errfileCanary(c *core.CliContext) error {
	if os.Getenv(EnvCanary) != "1" {
		return errors.New("the canary is inert: set " + EnvCanary + "=1")
	}

	if _, err := initializeSystem(c); err != nil {
		return err
	}

	errfile.Caught("running the errfile canary", errors.New("errfile canary: a deliberate fault in the app-cli runtime"), errfile.F("command", "utility __canary"))
	errfile.Flush()
	fmt.Printf("canary reported to %s\n", errfile.InstalledFile())

	return nil
}

// CommandName is the subcommand words of argv ("server run", "database update") for the main() net
// (pm/error_err.mdx §7 G8). Never the whole argv: a flag value could carry an address or a path.
func CommandName(argv []string) string {
	return strings.Join(commandWords(argv), " ")
}
