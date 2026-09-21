package a

import (
	"fmt"
	"os"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

// F1 — G1 violated: the error is replaced by a sentinel and nobody wrote it down
func f1() error {
	if _, err := os.Stat("x"); err != nil { // want "neither hands the error on"
		return errOperationFailed
	}

	return nil
}

// F2 — the block continues without the error
func f2() {
	for i := 0; i < 3; i++ {
		if _, err := os.Stat("x"); err != nil { // want "neither hands the error on"
			continue
		}
	}
}

// F3 — the block falls through
func f3() int {
	n := 0

	if _, err := os.Stat("x"); err != nil { // want "neither hands the error on"
		n = -1
	}

	return n
}

// F4 — the else of == nil swallows
func f4() string {
	_, err := os.Stat("x")

	if err == nil {
		return "ok"
	} else { // want "neither hands the error on"
		return "default"
	}
}

// F5 — a case clause that swallows
func f5() string {
	_, err := os.Stat("x")

	switch {
	case err != nil: // want "neither hands the error on"
		return "default"
	}

	return "ok"
}

// F6 — a compound condition that swallows
func f6() error {
	if _, err := os.Stat("x"); err != nil && true { // want "neither hands the error on"
		return errOperationFailed
	}

	return nil
}

// F7 — G4 violated: a discarded error result
func f7() {
	os.Remove("tmp") // want "is discarded"
}

// F8 — G4 violated: a multi-result call whose error is dropped
func f8() {
	os.Create("x") // want "is discarded"
}

// F9 — G5 violated: `_ = recover()`
func f9() {
	defer func() { _ = recover() }() // want "recovered value is dropped"
}

// F10 — G5 violated: a bare recover() statement
func f10() {
	defer func() { recover() }() // want "recovered value is dropped"
}

// F11 — G5 violated: the value is bound but never reaches a call
func f11() {
	defer func() {
		if r := recover(); r != nil { // want "never reaches errfile.Recovered"
			fmt.Println("panic ignored")
		}
	}()
}

// F12 — G5 violated: compared and dropped
func f12() {
	defer func() {
		if recover() != nil { // want "recovered value is dropped"
		}
	}()
}

// F13 — G6 violated: a naked go with a literal
func f13() {
	go func() { // want "a panic in this goroutine"
		_, _ = os.Stat("x")
	}()
}

// F14 — G6 violated: a naked go with a named function
func f14() {
	go f13() // want "a panic in this goroutine"
}

// F15 — dynamic doing: a Sprintf
func f15(name string) {
	errfile.Caught(fmt.Sprintf("loading %s", name), errOperationFailed) // want "must be a string literal"
}

// F16 — dynamic doing: a plain variable concatenation
func f16(name string) {
	errfile.Caught("loading "+name, errOperationFailed) // want "must be a string literal"
}

// F17 — a selector-typed error expression that is swallowed
func f17(r *result) string {
	if r.Err != nil { // want "neither hands the error on"
		return "default"
	}

	return "ok"
}

// F18 — dynamic doing on RecoverNet
func f18(name string) {
	go func() {
		defer errfile.RecoverNet("job " + name)() // want "must be a string literal"
	}()
}

// F19 — dynamic doing: an arbitrary method call
func f19(c *webCtx) {
	errfile.Caught("handling "+c.other(), errOperationFailed) // want "must be a string literal"
}

func (c *webCtx) other() string { return "" }
