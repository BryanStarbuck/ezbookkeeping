// Package b is the fixture for -errfile.report-sites: every site emits a `site` diagnostic too.
package b

import (
	"os"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

func sites() error {
	if _, err := os.Stat("x"); err != nil { // want "error site: error-nil block" "neither hands the error on"
		return nil
	}

	if _, err := os.Stat("y"); err != nil { // want "error site: error-nil block"
		errfile.Caught("checking y", err)
	}

	os.Remove("z") // want "error site: discarded error result" "is discarded"

	go func() { // want "error site: go statement"
		defer errfile.RecoverNet("working")()
	}()

	defer func() {
		if r := recover(); r != nil { // want "error site: recover"
			errfile.Recovered("running sites", r)
		}
	}()

	return nil
}
