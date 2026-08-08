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
	avd := flag.String("avd", "", "AVD name whose qemu-system-* to reap (REQUIRED per §11.4.174: an empty -avd refuses to reap, never a host-wide name match)")
	flag.Parse()

	// §11.4.174: refuse a host-wide reap. Cleanup requires an ownership token
	// (the AVD name); without -avd there is nothing we can prove is ours, so we
	// reap nothing and exit 0. Callers (run-emulator-tests.sh) MUST pass -avd
	// <name> per booted AVD to reap that AVD's stuck qemu-system-*.
	if *avd == "" {
		fmt.Fprintln(os.Stderr,
			"emulator-cleanup: no -avd supplied; refusing host-wide reap (§11.4.174). Pass -avd <name> to scope.")
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	report, err := emulator.Cleanup(ctx, *avd)
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
