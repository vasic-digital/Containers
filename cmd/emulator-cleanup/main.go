// Package main is the thin CLI wrapping pkg/emulator.Cleanup.
//
// Invoked by the consuming project's scripts/run-emulator-tests.sh as a pre-boot
// zombie-cleanup step. Best-effort: returns 0 even when no matches
// were found OR some PIDs survived (the matrix runner SHOULD continue
// regardless — the cleanup is a hygiene improvement, not a gate).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"digital.vasic.containers/pkg/emulator"
)

func main() {
	verbose := flag.Bool("verbose", false, "print full CleanupReport JSON to stdout")
	timeoutSec := flag.Int("timeout", 30, "overall timeout in seconds")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	report, err := emulator.Cleanup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "emulator-cleanup: %v\n", err)
		// Best-effort: even on error we exit 0 so the matrix runner can proceed.
		// Operator can spot the error in stderr.
		os.Exit(0)
	}

	fmt.Fprintf(os.Stderr,
		"emulator-cleanup: found=%d terminated=%d killed=%d surviving=%d skipped=%d\n",
		len(report.Found), len(report.TerminatedTERM), len(report.KilledKILL),
		len(report.Surviving), len(report.SkippedReadErr),
	)

	if *verbose {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(os.Stdout, string(b))
	}

	os.Exit(0)
}
