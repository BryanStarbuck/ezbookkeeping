package cron

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

type recordingSink struct{ records []*errfile.Record }

func (s *recordingSink) Write(r *errfile.Record) { s.records = append(s.records, r) }
func (s *recordingSink) Flush()                  {}

// pm/error_err.mdx §8 N10: a panicking job is one ERROR line and the scheduler survives.
func TestDoRunRecoversAPanickingJob(t *testing.T) {
	errfile.ResetForTests()
	t.Cleanup(errfile.ResetForTests)
	sink := &recordingSink{}
	errfile.InstallSinkForTests("server", sink)

	job := &CronJob{Name: "exploding_job", Period: CronJobIntervalPeriod{Interval: time.Minute}, Run: func(*core.CronContext) error { panic(errors.New("job exploded")) }}
	job.doRun()

	if len(sink.records) != 1 || sink.records[0].Doing != "running the cron job exploding_job" || !strings.Contains(sink.records[0].Error, "job exploded") {
		t.Errorf("records = %+v", sink.records)
	}
}
