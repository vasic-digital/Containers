# Containers - A generic, reusable Go module for container orchestration, health checking, lifecycle management, and service discovery. Supports Docker, Podman, and Kubernetes runtimes.
# Module: digital.vasic.containers

.PHONY: build test test-race test-short test-integration test-bench test-coverage fmt vet lint clean help \
	anti-bluff anti-bluff-scan anti-bluff-anchors anti-bluff-mutation anti-bluff-mutation-changed \
	update-baseline qa-all challenge

MODULE := digital.vasic.containers
GOMAXPROCS ?= 2

# P1.5-WP4: API-key loader. Sourced before build/test recipes so credentials
# from $HOME/api_keys.sh (or .env fallback) are available in the env. Guard
# keeps recipes working when the loader is absent.
LOAD_KEYS := if [ -f scripts/load_api_keys.sh ]; then . scripts/load_api_keys.sh; fi

build:
	@$(LOAD_KEYS); go build ./...

test:
	@$(LOAD_KEYS); GOMAXPROCS=$(GOMAXPROCS) go test -count=1 -race -p 1 ./...

test-race:
	GOMAXPROCS=$(GOMAXPROCS) go test -count=1 -race -p 1 ./...

test-short:
	GOMAXPROCS=$(GOMAXPROCS) go test -count=1 -short -p 1 ./...

test-integration:
	GOMAXPROCS=$(GOMAXPROCS) go test -count=1 -race -p 1 ./tests/integration/... 2>/dev/null || echo "No integration tests"

test-bench:
	GOMAXPROCS=$(GOMAXPROCS) go test -bench=. -benchmem ./tests/benchmark/... 2>/dev/null || echo "No benchmarks"

test-coverage:
	GOMAXPROCS=$(GOMAXPROCS) go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

fmt:
	gofmt -w .
	goimports -w .

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed"; exit 1; }
	golangci-lint run ./...

clean:
	rm -f coverage.out coverage.html
	go clean -cache

# Challenges (run from parent project)
challenge:
	../challenges/scripts/containers_challenge.sh 2>/dev/null || echo "No challenge script"

# === CONST-035 anti-bluff gates ===

anti-bluff-scan:
	@bash scripts/anti-bluff/bluff-scanner.sh --mode all

anti-bluff-anchors:
	@bash challenges/scripts/anchor_manifest_challenge.sh

anti-bluff-mutation:
	@bash challenges/scripts/mutation_ratchet_challenge.sh --mode all

anti-bluff-mutation-changed:
	@bash challenges/scripts/mutation_ratchet_challenge.sh

# SIGPIPE-V: a pipeline whose status decides a verdict while its last stage may
# close the read end early. Under `set -o pipefail` the writer dies of SIGPIPE
# and the check fails BECAUSE the pattern matched. Detector + its §1.1 paired
# mutation proof, whose mutations are fixture DATA, never edits to the detector.
sigpipe-verdict-scan:
	@bash scripts/anti-bluff/sigpipe-verdict-scanner.sh --mode all

sigpipe-verdict-proof:
	@bash scripts/anti-bluff/tests/run-sigpipe-fixtures.sh

sigpipe-verdict: sigpipe-verdict-scan sigpipe-verdict-proof

anti-bluff: anti-bluff-scan anti-bluff-anchors anti-bluff-mutation-changed sigpipe-verdict

update-baseline:
	@echo "Manual baseline update — see docs/ANTI_BLUFF.md"
	@echo "1. Run scanner: bash scripts/anti-bluff/bluff-scanner.sh --mode all"
	@echo "2. Run mutation: bash challenges/scripts/mutation_ratchet_challenge.sh --mode all"
	@echo "3. Edit challenges/baselines/bluff-baseline.txt to reflect new state."

# Aggregated quality gate. Existing pre-CONST-035 challenges (host-power, no-suspend)
# remain wired so qa-all asserts the union.
qa-all:
	@bash challenges/scripts/no_suspend_calls_challenge.sh
	@bash challenges/scripts/host_no_auto_suspend_challenge.sh
	@$(MAKE) anti-bluff

help:
	@echo "Containers - A generic, reusable Go module for container orchestration, health checking, lifecycle management, and service discovery. Supports Docker, Podman, and Kubernetes runtimes."
	@echo ""
	@echo "Build & Test:"
	@echo "  make build         Build all packages"
	@echo "  make test          Run all tests with race detection"
	@echo "  make test-short    Run unit tests only"
	@echo "  make test-bench    Run benchmarks"
	@echo "  make test-coverage Generate coverage report"
	@echo ""
	@echo "Quality:"
	@echo "  make fmt           Format code"
	@echo "  make vet           Run go vet"
	@echo "  make lint          Run golangci-lint"
	@echo ""
	@echo "Other:"
	@echo "  make clean         Remove build artifacts"
	@echo "  make challenge     Run challenge script (from parent project)"
