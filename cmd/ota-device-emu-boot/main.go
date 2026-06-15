// Command ota-device-emu-boot is a runnable example showing how a consumer
// brings up the OTA device-emulator image on-demand via the
// digital.vasic.containers pkg/boot + pkg/health surfaces, then health-checks
// the stack — satisfying the Constitution §11.4.76 on-demand-infra invariant
// (operators never run `podman compose up` by hand; boot is the entry point).
//
// Topology brought up (via a compose file the caller supplies with --compose):
//
//	control-plane   — the OTA server under test (HTTP health endpoint)
//	ota-device-emu  — THIS submodule's image, running the consumer-supplied
//	                  ota-device-emu binary, configured via OTA_* env to point
//	                  at the control plane and impersonate one device.
//
// Health model (honest): the control plane exposes an inbound HTTP health
// endpoint, so it is health-checked directly (pkg/health HTTP checker). The
// device emulator is an OUTBOUND client (it polls/heartbeats the control
// plane) with no inbound port, so its liveness is verified by asking the
// container runtime whether its container is running — NOT by a fake port
// probe. That is the §11.4 anti-bluff posture: assert the real observable.
//
// This file is an example/helper. It is decoupled from any specific consumer:
// every project-specific value (compose file, service names, health URL, OTA_*
// env) is a flag or comes from the compose file — nothing is hardcoded.
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
	"digital.vasic.containers/pkg/endpoint"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/runtime"
)

var (
	flagCompose       = flag.String("compose", "", "Path to the docker-compose file declaring the control-plane + ota-device-emu services (required)")
	flagProject       = flag.String("project", ".", "Project directory (compose build-context root)")
	flagControlName   = flag.String("control-service", "control-plane", "Compose service name of the OTA control plane")
	flagEmuName       = flag.String("emu-service", "ota-device-emu", "Compose service name of the OTA device emulator")
	flagControlHost   = flag.String("control-host", "localhost", "Host the control-plane health endpoint is reachable on")
	flagControlPort   = flag.String("control-port", "8080", "Port the control-plane health endpoint listens on")
	flagControlHealth = flag.String("control-health-path", "/health", "HTTP path for the control-plane health check")
	flagTimeout       = flag.Duration("timeout", 3*time.Minute, "Overall boot + health timeout")
	flagHelp          = flag.Bool("help", false, "Show help")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ota-device-emu-boot --compose <file> [options]\n\n")
		fmt.Fprintf(os.Stderr, "Bring up an OTA control-plane + ota-device-emu stack on-demand and health-check it.\n")
		fmt.Fprintf(os.Stderr, "The compose file MUST declare the two services and wire the ota-device-emu service's\n")
		fmt.Fprintf(os.Stderr, "OTA_* environment (OTA_BASE_URL pointing at the control plane, OTA_HARDWARE_ID, etc.).\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *flagHelp {
		flag.Usage()
		os.Exit(0)
	}
	if *flagCompose == "" {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "\nError: --compose is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *flagTimeout)
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\nShutting down...")
		cancel()
	}()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	logger := logging.NewStdLogger("ota-device-emu-boot")

	// 1. Auto-detect the local container runtime (Docker / Podman).
	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		return fmt.Errorf("auto-detect runtime: %w", err)
	}
	logger.Info("Using runtime: %s", rt.Name())

	// 2. Declare the endpoints. The control plane is the only inbound-port
	//    service, so it carries the HTTP health check. The ota-device-emu
	//    service is brought up by the same compose file but has no inbound
	//    health port — its liveness is verified at step 4 via the runtime.
	endpoints := map[string]endpoint.ServiceEndpoint{
		*flagControlName: endpoint.NewEndpoint().
			WithHost(*flagControlHost).
			WithPort(*flagControlPort).
			WithHealthType("http").
			WithHealthPath(*flagControlHealth).
			WithRequired(true).
			WithComposeFile(*flagCompose).
			WithServiceName(*flagControlName).
			Build(),
		*flagEmuName: endpoint.NewEndpoint().
			// No inbound port — disable the health probe for this endpoint;
			// liveness is asserted via the runtime at step 4 (anti-bluff).
			WithHealthType("custom").
			WithRequired(true).
			WithComposeFile(*flagCompose).
			WithServiceName(*flagEmuName).
			Build(),
	}

	// 3. Boot the whole stack on-demand (compose up + health). This is the
	//    §11.4.76 on-demand-infra entry point — no manual `compose up`.
	mgr := boot.NewBootManager(
		endpoints,
		boot.WithRuntime(rt),
		boot.WithLogger(logger),
		boot.WithProjectDir(*flagProject),
	)
	summary, err := mgr.BootAll(ctx)
	if err != nil {
		return fmt.Errorf("boot stack: %w", err)
	}
	logger.Info("Boot summary: started=%d remote=%d failed=%d skipped=%d",
		summary.Started, summary.Remote, summary.Failed, summary.Skipped)
	if summary.Failed > 0 {
		return fmt.Errorf("boot reported %d failed service(s)", summary.Failed)
	}

	// 4. Positive evidence, two ways:
	//    (a) the control plane answers its HTTP health endpoint;
	//    (b) the ota-device-emu container is genuinely RUNNING (queried from
	//        the runtime, not assumed) — this is the device-emulator's
	//        liveness oracle since it has no inbound port.
	checker := health.NewDefaultChecker()
	res := checker.Check(ctx, health.HealthTarget{
		Name:    *flagControlName,
		Host:    *flagControlHost,
		Port:    *flagControlPort,
		Type:    health.HealthHTTP,
		Path:    *flagControlHealth,
		Timeout: 10 * time.Second,
	})
	if !res.Healthy {
		return fmt.Errorf("control-plane health check FAILED: %s", res.Error)
	}
	logger.Info("Control-plane HTTP health: HEALTHY (%s)", res.Duration)

	st, err := rt.Status(ctx, *flagEmuName)
	if err != nil {
		return fmt.Errorf("query ota-device-emu container status: %w", err)
	}
	if st.State != runtime.StateRunning {
		return fmt.Errorf("ota-device-emu container is not running (state=%q)", st.State)
	}
	logger.Info("ota-device-emu container: RUNNING (id=%s)", st.ID)

	fmt.Println("OK: control plane healthy AND ota-device-emu running — stack is up.")
	return nil
}
