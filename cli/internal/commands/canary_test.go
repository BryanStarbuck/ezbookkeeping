package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
)

type canarySink struct{ records []*errfile.Record }

func (s *canarySink) Write(r *errfile.Record) { s.records = append(s.records, r) }
func (s *canarySink) Flush()                  {}

func TestCanaryVerbReportsOneFault(t *testing.T) {
	errfile.ResetForTests()
	t.Cleanup(errfile.ResetForTests)
	sink := &canarySink{}
	errfile.InstallSinkForTests("ezbk", sink)

	// the verb is hidden unless the env var was set at start-up; run its body directly
	var out bytes.Buffer

	if err := runCanary(&app.Ctx{Out: &out}); err != nil {
		t.Fatal(err)
	}

	if len(sink.records) != 1 || sink.records[0].App != "ezbk" || !strings.Contains(sink.records[0].Error, "errfile canary") {
		t.Errorf("records = %+v", sink.records)
	}

	for _, v := range app.All() {
		if v.Name == "__canary" {
			t.Errorf("the canary verb must not be registered without %s", EnvCanary)
		}
	}
}
