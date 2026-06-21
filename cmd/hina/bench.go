package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/RenanQueiroz/hina-agent/internal/bench"
)

// cmdBench runs the built-in live-voice turn-detection benchmark suite and prints
// per-fixture metrics. It is non-interactive and needs no models or assets — it
// drives the real turn-detection pipeline with a deterministic energy VAD, so it
// runs on every Tier-1 host (including the Windows CI runner). `--json` emits the
// raw results for CI ingestion.
func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit results as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	results := bench.RunSuite(bench.Fixtures(), bench.NewEnergyModel)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}

	fmt.Println("Live-voice turn-detection benchmark (synthetic VAD; real pipeline)")
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIXTURE\tONSET\tCOMMIT\tCANCEL\tBARGE\tFALSE_START\tMISSED\tFALSE_INT\tBC_SUPPR\tEOT_p50\tEOT_p90\tINT_p50")
	for _, r := range results {
		bc := "-"
		if r.BackchannelTotal > 0 {
			bc = fmt.Sprintf("%d/%d", r.BackchannelSuppressed, r.BackchannelTotal)
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n",
			r.Fixture, r.Opens, r.Commits, r.Cancels, r.BargeIns,
			r.FalseStarts, r.MissedStarts, r.FalseInterruptions, bc,
			ms(r.EndOfTurnDelayMs.P50), ms(r.EndOfTurnDelayMs.P90), ms(r.InterruptionDelayMs.P50))
	}
	tw.Flush()
	fmt.Println()
	fmt.Println("EOT = end-of-turn delay (commit minus truth speech end). INT = interruption delay. Latencies in ms.")
	return nil
}

// ms formats a millisecond stat, showing "-" for an empty sample set (0 count).
func ms(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}
