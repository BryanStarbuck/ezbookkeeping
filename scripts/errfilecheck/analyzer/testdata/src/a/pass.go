// Package a holds the fixtures of errfile/catch-must-report: pass.go has no diagnostics, fail.go
// has one per pattern.
package a

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/log"
)

var errOperationFailed = errors.New("operation failed")

const doingConst = "loading the constant"

type route struct {
	Method string
	Path   string
}

type verb struct{ Name string }

type webCtx struct{ Method string }

func (c *webCtx) FullPath() string { return "/api/v1/x/:id" }

// P1 — G1: the block reports through errfile before returning a sentinel
func p1() error {
	if err := os.Remove("x"); err != nil {
		errfile.Caught("removing the temporary file", err)
		return errOperationFailed
	}

	return nil
}

// P2 — G2: the error is handed on (return)
func p2() (int, error) {
	n, err := fmt.Sscanf("1", "%d", new(int))

	if err != nil {
		return 0, err
	}

	return n, nil
}

// P3 — G2: the error is handed on wrapped
func p3() error {
	if _, err := os.Stat("x"); err != nil {
		return fmt.Errorf("reading the manifest: %w", err)
	}

	return nil
}

// P4 — G2: the error is carried in an assignment
func p4() error {
	var lastErr error

	for i := 0; i < 3; i++ {
		if _, err := os.Stat("x"); err != nil {
			lastErr = err
			continue
		}
	}

	return lastErr
}

// P5 — G3: the failure is marked expected
func p5() string {
	if _, err := os.ReadFile("optional"); err != nil {
		errfile.Expected("reading the optional statements root", err)
		return "default"
	}

	return "read"
}

// P6 — G1 (unchanged): the block already reports through pkg/log (net-covered)
func p6() error {
	if _, err := os.Stat("x"); err != nil {
		log.Errorf(nil, "[a.p6] failed to stat, because %s", err.Error())
		return errOperationFailed
	}

	return nil
}

// P7 — the else of == nil hands the error on
func p7() error {
	_, err := os.Stat("x")

	if err == nil {
		return nil
	} else {
		return err
	}
}

// P8 — a compound condition with an errors.Is, reported
func p8() error {
	if _, err := os.Stat("x"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errfile.Caught("reading the sidecar", err, errfile.F("file", "x"))
		return errOperationFailed
	}

	return nil
}

// P9 — G4: a discarded result made explicit, and a defer Close
func p9() {
	_ = os.Remove("tmp")
	f, err := os.Open("x")

	if err != nil {
		errfile.Warn("opening the file", err)
		return
	}

	defer f.Close()
}

// P10 — G5: recover reaches errfile.Recovered
func p10() {
	defer func() {
		if r := recover(); r != nil {
			errfile.Recovered("running the job", r)
		}
	}()
}

// P11 — G5: recover reaches panic (re-panic) via a call argument
func p11() {
	defer func() {
		r := recover()

		if r != nil {
			panic(r)
		}
	}()
}

// P12 — G6: errfile.Go is a plain call, never a go statement
func p12() {
	errfile.Go("sending the verification email", func() {})
}

// P13 — G6: a plain go whose literal starts with the net
func p13() {
	go func() {
		defer errfile.RecoverNet("removing rotated log files")()
	}()
}

// P14 — the doing forms: literal, const, route selectors, verbName
func p14(r *route, v *verb) {
	verbName := v.Name
	errfile.Caught(doingConst, errOperationFailed)
	errfile.Caught("handling "+r.Method+" "+r.Path, errOperationFailed)
	errfile.Caught("running "+verbName, errOperationFailed)
	errfile.Caught("running the cron job "+v.Name, errOperationFailed)
	c := &webCtx{}
	errfile.Recovered("handling "+c.Method+" "+c.FullPath(), "x")
}

// P15 — a case clause that hands the error on
func p15() error {
	_, err := os.Stat("x")

	switch {
	case err != nil:
		return err
	}

	return nil
}

// P16 — panic inside the block counts as reporting
func p16() {
	if _, err := os.Stat("x"); err != nil {
		panic("cannot continue")
	}
}

// P17 — printing never fails: not a discarded error
func p17() {
	var b strings.Builder
	b.WriteString("x")
	fmt.Println("hello")
	fmt.Fprintf(os.Stderr, "x")
}

// P18 — recover() passed directly to a call
func p18() {
	defer func() {
		errfile.Recovered("running", recover())
	}()
}

// P19 — the error identifier in a call argument hands it on
func p19() error {
	if _, err := os.Stat("x"); err != nil {
		return fmt.Errorf("stat: %v", err.Error())
	}

	return nil
}

// P20 — a selector-typed error expression that is handed on
type result struct{ Err error }

func p20(r *result) error {
	if r.Err != nil {
		return r.Err
	}

	return nil
}

// P21 — a thin wrapper may pass its own `doing` parameter through
func p21(doing string, err error) {
	errfile.Caught(doing, err)
}
