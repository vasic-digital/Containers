// Command deploy-stack deploys a docker/podman compose stack to a remote
// host over SSH using this module's pkg/remote orchestration primitives.
//
// It is the generic, project-agnostic complement to cmd/boot: where boot
// distributes individual containers per a scheduler, deploy-stack ships a
// single compose project (the compose file plus its build context inputs)
// to ONE remote host and runs `compose up -d --build` there. No consuming
// project's names, ports, or paths are baked in — every input is a flag or
// an env-file value (per the module's decoupling mandate).
//
// Flow:
//  1. Resolve the target host from CONTAINERS_REMOTE_HOST_<N>_* in --env.
//  2. scp every --artifact (compose file, Containerfile, entrypoint,
//     init scripts, certs dir, the env file) into --remote-dir on the host.
//  3. Construct an SSHExecutor + RemoteComposeOrchestrator and run Up,
//     which executes `<compose> -f <remote compose> up -d --build`.
//  4. Print the resulting service status (compose ps) so the caller sees
//     what actually came up — not merely that the command returned 0.
//
// Usage:
//
//	deploy-stack \
//	  --env ./.env.nezha \
//	  --host-index 1 \
//	  --compose ./compose.nezha.yml \
//	  --remote-dir helixtranslate \
//	  --artifact Containerfile.nezha \
//	  --artifact scripts/nezha-entrypoint.sh \
//	  --artifact scripts/init-db.sql \
//	  --artifact-dir certs \
//	  --env-file ./.env.nezha
//
// The compose file basename and every --artifact basename land directly in
// --remote-dir; --artifact-dir copies a whole directory; --env-file is the
// secret env file the compose `env_file:` directive reads (its basename is
// preserved so compose resolves it).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// stringSlice collects repeated flag values.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var (
		flagEnv        = flag.String("env", "", "Path to env file holding CONTAINERS_REMOTE_HOST_<N>_* (required)")
		flagHostIndex  = flag.Int("host-index", 1, "Which CONTAINERS_REMOTE_HOST_<N> block to target")
		flagCompose    = flag.String("compose", "", "Path to the local compose file to deploy (required)")
		flagRemoteDir  = flag.String("remote-dir", "helix-stack", "Remote directory (relative to the SSH login dir) to deploy into")
		flagEnvUpload  = flag.String("env-file", "", "Path to the secret env file the compose env_file: reads (uploaded, basename preserved)")
		flagProject    = flag.String("project-name", "", "compose --project-name (default: compose file basename without extension)")
		flagComposeCmd = flag.String("compose-cmd", "", "Force a compose command (e.g. podman-compose); empty = auto-detect")
		flagTimeout    = flag.Duration("timeout", 30*time.Minute, "Overall deploy timeout (image build can take many minutes)")
		flagStrictKey  = flag.Bool("strict-host-key", false, "Enable SSH StrictHostKeyChecking")
		flagHelp       = flag.Bool("help", false, "Show help")
	)
	var artifacts stringSlice
	var artifactDirs stringSlice
	flag.Var(&artifacts, "artifact", "Local file to scp into --remote-dir (repeatable)")
	flag.Var(&artifactDirs, "artifact-dir", "Local directory to scp into --remote-dir (repeatable)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: deploy-stack [options]\n\n")
		fmt.Fprintf(os.Stderr, "Deploy a compose stack to a remote host over SSH and run compose up -d --build.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *flagHelp {
		flag.Usage()
		return
	}
	if *flagEnv == "" || *flagCompose == "" {
		fmt.Fprintln(os.Stderr, "error: --env and --compose are required")
		flag.Usage()
		os.Exit(2)
	}

	logger := logging.NewStdLogger("deploy-stack")

	if err := run(*flagEnv, *flagHostIndex, *flagCompose, *flagRemoteDir,
		*flagEnvUpload, *flagProject, *flagComposeCmd, *flagTimeout,
		*flagStrictKey, artifacts, artifactDirs, logger); err != nil {
		fmt.Fprintf(os.Stderr, "deploy-stack: %v\n", err)
		os.Exit(1)
	}
}

func run(
	envPath string, hostIndex int, composePath, remoteDir, envUpload,
	projectName, composeCmd string, timeout time.Duration, strictKey bool,
	artifacts, artifactDirs []string, logger logging.Logger,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Honor Ctrl-C / SIGTERM by cancelling the context.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("signal received — cancelling deploy")
		cancel()
	}()

	env, err := loadEnv(envPath)
	if err != nil {
		return fmt.Errorf("load env %s: %w", envPath, err)
	}

	host, err := hostFromEnv(env, hostIndex)
	if err != nil {
		return err
	}
	logger.Info("target host: %s (%s@%s) runtime=%s",
		host.Name, host.User, host.Address, host.Runtime)

	exec, err := remote.NewSSHExecutor(logger,
		remote.WithCommandTimeout(timeout),
		remote.WithStrictHostKeyCheck(strictKey),
	)
	if err != nil {
		return fmt.Errorf("create ssh executor: %w", err)
	}

	// 0. Ensure the remote dir exists.
	if _, err := exec.Execute(ctx, host,
		fmt.Sprintf("mkdir -p %s", shellQuote(remoteDir))); err != nil {
		return fmt.Errorf("mkdir remote dir: %w", err)
	}

	// 1. Copy the compose file.
	composeBase := filepath.Base(composePath)
	if err := exec.CopyFile(ctx, host, composePath,
		filepath.Join(remoteDir, composeBase)); err != nil {
		return fmt.Errorf("copy compose file: %w", err)
	}
	logger.Info("copied %s -> %s/%s", composePath, remoteDir, composeBase)

	// 2. Copy the secret env file (basename preserved so env_file: resolves).
	if envUpload != "" {
		base := filepath.Base(envUpload)
		if err := exec.CopyFile(ctx, host, envUpload,
			filepath.Join(remoteDir, base)); err != nil {
			return fmt.Errorf("copy env file: %w", err)
		}
		logger.Info("copied env file -> %s/%s", remoteDir, base)
	}

	// 3. Copy each artifact file (preserving any subdir under remote-dir).
	for _, a := range artifacts {
		rel := a
		dest := filepath.Join(remoteDir, rel)
		// Ensure parent dir exists for nested artifact paths.
		if d := filepath.Dir(dest); d != "." && d != remoteDir {
			if _, err := exec.Execute(ctx, host,
				fmt.Sprintf("mkdir -p %s", shellQuote(d))); err != nil {
				return fmt.Errorf("mkdir for artifact %s: %w", a, err)
			}
		}
		if err := exec.CopyFile(ctx, host, a, dest); err != nil {
			return fmt.Errorf("copy artifact %s: %w", a, err)
		}
		logger.Info("copied artifact %s -> %s", a, dest)
	}

	// 4. Copy each artifact directory.
	for _, d := range artifactDirs {
		dest := filepath.Join(remoteDir, filepath.Base(d))
		if _, err := exec.Execute(ctx, host,
			fmt.Sprintf("mkdir -p %s", shellQuote(dest))); err != nil {
			return fmt.Errorf("mkdir artifact-dir %s: %w", d, err)
		}
		if err := exec.CopyDir(ctx, host, d, dest); err != nil {
			return fmt.Errorf("copy artifact-dir %s: %w", d, err)
		}
		logger.Info("copied artifact-dir %s -> %s", d, dest)
	}

	// 5. Build orchestrator + run Up. The compose file is referenced by its
	//    absolute remote path so the compose tool resolves build contexts
	//    (build: { context: . }) relative to that file's directory.
	remoteHome, err := exec.Execute(ctx, host, "pwd")
	if err != nil {
		return fmt.Errorf("resolve remote home: %w", err)
	}
	absRemoteDir := filepath.Join(strings.TrimSpace(remoteHome.Stdout), remoteDir)
	absCompose := filepath.Join(absRemoteDir, composeBase)

	if projectName == "" {
		projectName = strings.TrimSuffix(composeBase, filepath.Ext(composeBase))
		projectName = strings.NewReplacer(".", "-", "_", "-").Replace(projectName)
	}

	var orchOpts []remote.RemoteComposeOption
	if composeCmd != "" {
		orchOpts = append(orchOpts, remote.WithComposeCommand(composeCmd))
	}
	orch := remote.NewRemoteComposeOrchestrator(host, exec, logger, orchOpts...)

	project := compose.ComposeProject{
		Name: projectName,
		File: absCompose,
	}

	logger.Info("running compose up -d --build on %s (project=%s, file=%s)",
		host.Name, projectName, absCompose)
	if err := orch.Up(ctx, project); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	logger.Info("compose up completed")

	// 6. Report what actually came up (compose ps), not just exit 0.
	statuses, err := orch.Status(ctx, project)
	if err != nil {
		logger.Error("status query failed: %v", err)
	} else {
		fmt.Printf("\n=== %s stack status (%d services) ===\n", host.Name, len(statuses))
		for _, s := range statuses {
			fmt.Printf("  %-32s state=%-10s health=%-10s %s\n",
				s.Name, s.State, s.Health, strings.Join(s.Ports, ","))
		}
	}

	return nil
}

// loadEnv reads a KEY=VALUE env file into a map. Lines that are blank or
// start with '#' are ignored. Values are taken verbatim after the first '='.
func loadEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	env := make(map[string]string)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := line[eq+1:]
		env[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return env, nil
}

// hostFromEnv builds a RemoteHost from CONTAINERS_REMOTE_HOST_<N>_* keys.
func hostFromEnv(env map[string]string, n int) (remote.RemoteHost, error) {
	prefix := fmt.Sprintf("CONTAINERS_REMOTE_HOST_%d_", n)
	name := env[prefix+"NAME"]
	addr := env[prefix+"ADDRESS"]
	user := env[prefix+"USER"]
	if name == "" || addr == "" || user == "" {
		return remote.RemoteHost{}, fmt.Errorf(
			"env missing %sNAME/%sADDRESS/%sUSER", prefix, prefix, prefix)
	}
	h := remote.RemoteHost{
		Name:    name,
		Address: addr,
		User:    user,
		Runtime: env[prefix+"RUNTIME"],
		KeyPath: env[prefix+"KEY"],
	}
	if p := env[prefix+"PORT"]; p != "" {
		var port int
		if _, err := fmt.Sscanf(p, "%d", &port); err == nil {
			h.Port = port
		}
	}
	return h, nil
}

// shellQuote single-quotes a path for safe use inside a remote shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
