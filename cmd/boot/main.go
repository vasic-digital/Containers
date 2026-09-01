package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"digital.vasic.containers/pkg/boot"
	"digital.vasic.containers/pkg/distribution"
	"digital.vasic.containers/pkg/endpoint"
	"digital.vasic.containers/pkg/envconfig"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/runtime"
	"digital.vasic.containers/pkg/scheduler"
)

var (
	flagEnvFile = flag.String("env", "", "Path to .env file (default: ./pkg/envconfig/.env)")
	flagProject = flag.String("project", "", "Path to project directory")
	flagTimeout = flag.Duration("timeout", 5*time.Minute, "Boot timeout")
	flagHelp    = flag.Bool("help", false, "Show help")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: boot [options]\n\n")
		fmt.Fprintf(os.Stderr, "Boot services using the Containers module.\n")
		fmt.Fprintf(os.Stderr, "Distributes containers to remote hosts based on .env configuration.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  boot                           # Boot with default .env\n")
		fmt.Fprintf(os.Stderr, "  boot --env /path/to/.env      # Custom env file\n")
		fmt.Fprintf(os.Stderr, "  boot --project /my/project     # Custom project path\n")
	}

	flag.Parse()

	if *flagHelp {
		flag.Usage()
		os.Exit(0)
	}

	envFile := *flagEnvFile
	if envFile == "" {
		locations := []string{
			"../../../tools/containers/.env",
			"../../.env",
			"../.env",
			"./.env",
		}
		for _, loc := range locations {
			if _, err := os.Stat(loc); err == nil {
				envFile = loc
				break
			}
		}
	}

	projectDir := *flagProject
	if projectDir == "" {
		projectDir = "../../../"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		cancel()
	}()

	if err := runBoot(ctx, envFile, projectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// There is deliberately no local distributorAdapter here. One used to exist,
// embedding *distribution.DefaultDistributor and overriding DistributeEndpoints
// with `return 0, nil`. Because the override shadowed the embedded method,
// remote distribution deployed nothing and reported success to BootAll.
// *distribution.DefaultDistributor already satisfies boot.Distributor — its own
// doc comment says so — so it is passed straight through.

func runBoot(ctx context.Context, envFile, projectDir string) error {
	logger := logging.NewStdLogger("boot")
	logger.Info("Starting boot process...")

	var cfg *envconfig.DistributionConfig
	var err error

	if envFile != "" {
		cfg, err = envconfig.LoadFromFile(envFile)
		if err != nil {
			return fmt.Errorf("load env config: %w", err)
		}
	} else {
		cfg = envconfig.LoadFromEnv()
		logger.Info("No .env file found, using local mode")
	}

	logger.Info("Configuration loaded: remote=%v, hosts=%d, scheduler=%s",
		cfg.Enabled, len(cfg.Hosts), cfg.Scheduler)

	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		return fmt.Errorf("auto-detect runtime: %w", err)
	}
	logger.Info("Using runtime: %s", rt.Name())

	exec, err := remote.NewSSHExecutor(logger)
	if err != nil {
		return fmt.Errorf("create SSH executor: %w", err)
	}

	hostManager := remote.NewHostManager(exec, logger)
	for _, host := range cfg.ToRemoteHosts() {
		if err := hostManager.AddHost(host); err != nil {
			logger.Warn("Failed to add host %s: %v", host.Name, err)
			continue
		}
		logger.Info("Registered remote host: %s (%s)", host.Name, host.Address)
	}

	sched := scheduler.NewScheduler(hostManager, logger)

	var distributor boot.Distributor
	if cfg.Enabled && len(cfg.Hosts) > 0 {
		defaultDist := distribution.NewDistributor(
			distribution.WithScheduler(sched),
			distribution.WithHostManager(hostManager),
			distribution.WithLogger(logger),
		)
		distributor = defaultDist
		logger.Info("Remote distribution enabled with scheduler: %s", cfg.Scheduler)
	}

	// This module is consumer-agnostic (§11.4.28(B)) and may carry no host or
	// port literal in tracked source (§6.R). The endpoint list that used to sit
	// here was a hardcoded consumer service name pointing at a hardcoded
	// host:port, and it was the only thing this command ever tried to boot.
	//
	// Honest boundary: there is currently NO configuration source that supplies
	// service endpoints to this command — envconfig carries remote *hosts*, not
	// services — and BootAll's compose and health phases are inert here anyway,
	// because no orchestrator and no health checker are wired below. So this
	// command cannot presently boot anything, and it now says that instead of
	// printing a completion line over an empty result set.
	endpoints := map[string]endpoint.ServiceEndpoint{}

	if len(endpoints) == 0 {
		return fmt.Errorf(
			"no service endpoints are configured, so there is nothing to boot: " +
				"this command has no endpoint configuration source yet, and no " +
				"compose orchestrator or health checker is wired into its " +
				"BootManager. Use the pkg/boot API directly and pass " +
				"boot.WithOrchestrator/WithHealthChecker with your own endpoints")
	}

	bm := boot.NewBootManager(
		endpoints,
		boot.WithRuntime(rt),
		boot.WithLogger(logger),
		boot.WithProjectDir(projectDir),
		boot.WithDistributor(distributor),
		boot.WithHostManager(hostManager),
		boot.WithScheduler(sched),
	)

	logger.Info("Booting services...")
	bootCtx, bootCancel := context.WithTimeout(ctx, *flagTimeout)
	defer bootCancel()

	result, err := bm.BootAll(bootCtx)
	if err != nil {
		logger.Error("Boot failed: %v", err)
		return err
	}

	// A boot that processed nothing is not a boot. Reporting success here is
	// what let this command exit 0 having started no containers at all: every
	// phase is skipped when its collaborator is nil, and an empty result set
	// then reads exactly like a clean run.
	if len(result.Results) == 0 {
		return fmt.Errorf(
			"boot processed 0 services: %d configured endpoint(s) produced no "+
				"result, which means every phase was skipped rather than run. "+
				"Nothing was started", len(endpoints))
	}

	logger.Info("Boot completed: %d services processed", len(result.Results))
	for name, res := range result.Results {
		logger.Info("  - %s: %s (duration=%s)", name, res.Status, res.Duration)
	}

	return nil
}
