// Command errfilecheck is the Go half of errfile/catch-must-report (pm/error_err.mdx §13.1). It
// runs standalone (`bin/errfilecheck -json -errfile.report-sites ./...`, which the coverage script
// uses) and as a vet tool (`go vet -vettool=bin/errfilecheck ./...`).
package main

import (
	"os"
	"strings"

	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/BryanStarbuck/ezbookkeeping/scripts/errfilecheck/analyzer"
)

func main() {
	// go vet spells an analyzer's flag `-errfile.report-sites`; singlechecker registers it as
	// `-report-sites`. Accept the vet spelling standalone too, so one command line works for both.
	for i, a := range os.Args {
		if strings.HasPrefix(a, "-errfile.") {
			os.Args[i] = "-" + strings.TrimPrefix(a, "-errfile.")
		} else if strings.HasPrefix(a, "--errfile.") {
			os.Args[i] = "-" + strings.TrimPrefix(a, "--errfile.")
		}
	}

	singlechecker.Main(analyzer.Analyzer)
}
