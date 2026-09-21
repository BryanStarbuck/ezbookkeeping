// Package log is a stub of pkg/log for the analyzer's fixtures.
package log

func Errorf(c any, format string, args ...any)                        {}
func Warnf(c any, format string, args ...any)                         {}
func Infof(c any, format string, args ...any)                         {}
func ErrorfWithExtra(c any, extra string, format string, args ...any) {}
func BootErrorf(c any, format string, args ...any)                    {}
