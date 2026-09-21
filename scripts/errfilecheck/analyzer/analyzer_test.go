package analyzer

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "a")
}

func TestReportSitesFlag(t *testing.T) {
	reportSites = true
	t.Cleanup(func() { reportSites = false })
	// With sites on, every site emits one extra `site` diagnostic beside any violation.
	analysistest.Run(t, analysistest.TestData(), Analyzer, "b")
}
