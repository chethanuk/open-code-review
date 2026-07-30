//go:build windows

package llm

import "testing"

// TEMPORARY PROBE -- not for merge. Records what cmd.exe /S /C actually does
// with the quote shapes from the PR 605 review finding, so the answer is an
// observation rather than a reading of `cmd /?`.
//
// resolveKeyCmd reports "produced multi-line output" whenever the child printed
// two lines, which is exactly the signal for "cmd split my command line into two
// commands", so the returned error distinguishes the cases on its own.
func TestProbe_CmdExeQuoteSemantics(t *testing.T) {
	probes := []struct {
		name string
		cmd  string
	}{
		// The reviewer's PoC shape: trailing `& "` after a bare closing quote.
		{name: "poc_from_review", cmd: `echo A" & echo B & "`},
		// Doubled quote closes the region, so the following & should be unquoted.
		{name: "doubled_quote_then_amp", cmd: `echo A"" & echo B`},
		// Unbalanced single quote, nothing after it.
		{name: "unbalanced_trailing", cmd: `echo A" & echo B`},
		// Baseline: the documented working case.
		{name: "baseline_quoted_arg", cmd: `echo sk-"a b"-token`},
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			got, err := resolveKeyCmd(p.cmd, "probe")
			t.Logf("PROBE %-24s cmd=%q\n            -> out=%q\n            -> err=%v", p.name, p.cmd, got, err)
		})
	}
}
