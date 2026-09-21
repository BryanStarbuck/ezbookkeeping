// Package errfile is a stub of pkg/errfile for the analyzer's fixtures.
package errfile

type Field struct {
	Key   string
	Value any
}

func F(key string, value any) Field                    { return Field{Key: key, Value: value} }
func Caught(doing string, err error, fields ...Field)  {}
func Warn(doing string, err error, fields ...Field)    {}
func Expected(doing string, err error)                 {}
func Fatal(doing string, err error, fields ...Field)   {}
func Recovered(doing string, rec any, fields ...Field) {}
func Go(doing string, fn func())                       { go fn() }
func RecoverNet(doing string) func()                   { return func() { _ = recover() } }
