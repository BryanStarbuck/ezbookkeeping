package commands

// canary.go — the hidden `ezbk __canary` verb of pm/error_err.mdx §15.3. It exists only when
// EZBK_ERROR_FILE_CANARY=1 is set at start-up (the registry never sees it otherwise, so it is
// absent from help), and it reports one deliberate fault so the error file can be checked end to
// end for the ezbk runtime.

import (
	"errors"
	"fmt"
	"os"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
)

// EnvCanary switches the hidden canary verb on
const EnvCanary = "EZBK_ERROR_FILE_CANARY"

func init() {
	if os.Getenv(EnvCanary) != "1" {
		return
	}

	app.Register(app.Verb{
		Name:     "__canary",
		Summary:  "hidden: report one deliberate fault to ~/T/ezbookkeeping/error.err",
		Group:    "HIDDEN",
		MaxArgs:  0,
		NoServer: true,
		Run:      runCanary,
	})
}

func runCanary(c *app.Ctx) error {
	errfile.Caught("running the errfile canary", errors.New("errfile canary: a deliberate fault in ezbk"), errfile.F("verb", "__canary"))
	errfile.Flush()
	fmt.Fprintf(c.Out, "canary reported to %s\n", errfile.InstalledFile())

	return nil
}
