// Package policy supplies the resource-cap policy used to keep container
// stacks from exhausting host memory and SIGKILLing the user session.
//
// It is the Go counterpart of scripts/resource-policy/policy.yaml — both
// reference the same rules table and the same defaults. Either form can be
// the source of truth in a project; pick one. If the project uses both,
// run [VerifyAgainstYAML] in your test suite to keep them aligned.
//
// Caps are emitted in compose form (`mem_limit`, `memswap_limit`,
// `pids_limit`, `oom_score_adj`) and as a [Cap] struct that can be applied
// programmatically through the runtime layer.
package policy

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Cap describes the resource cap for a single container.
//
// Mem and Memswap use compose-style suffixes (`512m`, `2g`); Bytes returns
// them as raw byte counts. Pids is the cgroup `pids.max` value. OOMAdj is
// the kernel's `oom_score_adj` (positive ≈ preferred OOM victim).
type Cap struct {
	Mem     string // e.g. "2g"
	Memswap string // e.g. "2g"; >= Mem
	Pids    int    // pids_limit
	OOMAdj  int    // oom_score_adj (positive = preferred victim)
}

// Bytes returns Mem and Memswap parsed into raw bytes.
func (c Cap) Bytes() (mem, memswap uint64, err error) {
	if mem, err = parseSize(c.Mem); err != nil {
		return 0, 0, fmt.Errorf("Mem: %w", err)
	}
	if memswap, err = parseSize(c.Memswap); err != nil {
		return 0, 0, fmt.Errorf("Memswap: %w", err)
	}
	if memswap < mem {
		return 0, 0, fmt.Errorf("memswap (%d) < mem (%d)", memswap, mem)
	}
	return mem, memswap, nil
}

// Validate confirms the cap is internally consistent.
func (c Cap) Validate() error {
	mem, memswap, err := c.Bytes()
	if err != nil {
		return err
	}
	if mem < 64*1024*1024 {
		return fmt.Errorf("mem %s < 64m floor (likely a typo)", c.Mem)
	}
	if memswap < mem {
		return fmt.Errorf("memswap %s < mem %s", c.Memswap, c.Mem)
	}
	if c.Pids < 64 {
		return fmt.Errorf("pids_limit %d < 64", c.Pids)
	}
	if c.OOMAdj <= 0 {
		return errors.New("oom_score_adj must be positive so containers " +
			"are preferred OOM-killer victims (negative would protect " +
			"the container at the cost of the user manager)")
	}
	if c.OOMAdj > 1000 {
		return fmt.Errorf("oom_score_adj %d exceeds the kernel-valid maximum "+
			"of 1000 (a write >1000 to /proc/<pid>/oom_score_adj is rejected "+
			"with EINVAL, so the cap could never be applied)", c.OOMAdj)
	}
	return nil
}

// Rule binds a glob pattern (matched against the lowercased service name)
// to a Cap. Earlier rules win.
type Rule struct {
	Match string
	Cap   Cap
}

// Policy is an ordered set of [Rule]s plus a default cap used when nothing
// matches.
type Policy struct {
	Default Cap
	Rules   []Rule
}

// CapFor returns the cap to apply to a service named svc.
//
// Match is case-insensitive and uses [filepath.Match] glob semantics
// (“*“ and “?“). Earlier rules win, so put the most specific patterns
// at the top of [Policy.Rules].
func (p *Policy) CapFor(svc string) Cap {
	name := strings.ToLower(svc)
	for _, r := range p.Rules {
		ok, err := filepath.Match(strings.ToLower(r.Match), name)
		if err == nil && ok {
			return r.Cap
		}
	}
	return p.Default
}

// Validate runs Cap.Validate on every rule's cap and the default, and
// confirms every rule's Match pattern is syntactically valid.
//
// A malformed glob is NOT rejected by [filepath.Match] with a useful
// signal at match time: CapFor's `ok, err := filepath.Match(...); if err
// == nil && ok` treats a syntax error identically to "no match" (both
// leave ok false), so the affected service silently falls through to
// Default forever while Validate() kept reporting the policy as valid.
// Probing the pattern here (against an arbitrary, always-non-matching
// input) surfaces the syntax error at policy-load time instead.
func (p *Policy) Validate() error {
	if err := p.Default.Validate(); err != nil {
		return fmt.Errorf("default cap: %w", err)
	}
	for i, r := range p.Rules {
		if _, err := filepath.Match(strings.ToLower(r.Match), ""); err != nil {
			return fmt.Errorf("rule %d (%q): invalid Match pattern: %w", i, r.Match, err)
		}
		if err := r.Cap.Validate(); err != nil {
			return fmt.Errorf("rule %d (%q): %w", i, r.Match, err)
		}
	}
	return nil
}

// Default returns the policy bundled with this module. It is identical to
// the YAML at “scripts/resource-policy/policy.yaml“; run the matching
// test suite in CI/locally to keep them in sync.
//
// Numbers reflect a 62 GiB / 16 GiB-swap host (typical workstation) where
// the user GUI session must always have ~12 GiB of headroom. Tune in your
// own fork as appropriate, but never set OOMAdj <= 0.
func Default() Policy {
	return Policy{
		Default: Cap{Mem: "2g", Memswap: "2g", Pids: 1024, OOMAdj: 500},
		Rules: []Rule{
			// LLM inference (heaviest)
			{"ollama*", Cap{"12g", "12g", 2048, 500}},
			{"*tensorrt*", Cap{"12g", "12g", 2048, 500}},
			{"*triton*", Cap{"12g", "12g", 2048, 500}},
			{"vllm*", Cap{"12g", "12g", 2048, 500}},
			{"*-llm-*", Cap{"8g", "8g", 1024, 500}},

			// Browser-based MCP servers
			{"mcp-puppeteer*", Cap{"3g", "3g", 4096, 500}},
			{"mcp-playwright*", Cap{"3g", "3g", 4096, 500}},
			{"mcp-browserbase*", Cap{"3g", "3g", 4096, 500}},
			{"mcp-firecrawl*", Cap{"2g", "2g", 2048, 500}},

			// Image / ML MCP servers
			{"mcp-stable-diffusion*", Cap{"4g", "4g", 1024, 500}},
			{"mcp-imagesorcery*", Cap{"3g", "3g", 1024, 500}},
			{"mcp-vision*", Cap{"3g", "3g", 1024, 500}},

			// Generic MCP servers
			{"mcp-*", Cap{"1g", "1g", 1024, 500}},

			// Search engines
			{"elasticsearch*", Cap{"6g", "6g", 2048, 500}},
			{"opensearch*", Cap{"6g", "6g", 2048, 500}},
			{"logstash*", Cap{"3g", "3g", 1024, 500}},
			{"kibana*", Cap{"2g", "2g", 1024, 500}},

			// SonarQube
			{"sonarqube*", Cap{"8g", "8g", 2048, 500}},

			// Vector DBs
			{"qdrant*", Cap{"4g", "4g", 1024, 500}},
			{"weaviate*", Cap{"4g", "4g", 1024, 500}},
			{"chroma*", Cap{"3g", "3g", 1024, 500}},
			{"milvus*", Cap{"6g", "6g", 1024, 500}},
			{"vespa*", Cap{"6g", "6g", 1024, 500}},

			// SQL / NoSQL DBs
			{"postgres*", Cap{"4g", "4g", 1024, 500}},
			{"*-postgres", Cap{"4g", "4g", 1024, 500}},
			{"mysql*", Cap{"4g", "4g", 1024, 500}},
			{"mariadb*", Cap{"4g", "4g", 1024, 500}},
			{"mongodb*", Cap{"4g", "4g", 1024, 500}},
			{"mongo*", Cap{"4g", "4g", 1024, 500}},
			{"clickhouse*", Cap{"8g", "8g", 1024, 500}},
			{"cassandra*", Cap{"6g", "6g", 1024, 500}},
			{"neo4j*", Cap{"4g", "4g", 2048, 500}},

			// Caches
			{"redis*", Cap{"1g", "1g", 1024, 500}},
			{"memcached*", Cap{"1g", "1g", 1024, 500}},
			{"valkey*", Cap{"1g", "1g", 1024, 500}},

			// Messaging
			{"kafka*", Cap{"3g", "3g", 2048, 500}},
			{"rabbitmq*", Cap{"2g", "2g", 1024, 500}},
			{"nats*", Cap{"1g", "1g", 1024, 500}},
			{"pulsar*", Cap{"4g", "4g", 1024, 500}},
			{"zookeeper*", Cap{"1g", "1g", 1024, 500}},

			// Object storage
			{"minio*", Cap{"4g", "4g", 1024, 500}},
			{"*-minio", Cap{"4g", "4g", 1024, 500}},

			// Observability
			{"prometheus*", Cap{"2g", "2g", 1024, 500}},
			{"grafana*", Cap{"1g", "1g", 1024, 500}},
			{"loki*", Cap{"2g", "2g", 1024, 500}},
			{"tempo*", Cap{"2g", "2g", 1024, 500}},
			{"jaeger*", Cap{"1g", "1g", 1024, 500}},
			{"alertmanager*", Cap{"512m", "512m", 1024, 500}},
			{"node-exporter*", Cap{"256m", "256m", 1024, 500}},
			{"cadvisor*", Cap{"512m", "512m", 1024, 500}},
			{"otel-collector*", Cap{"1g", "1g", 1024, 500}},

			// Reverse proxies / gateways
			{"nginx*", Cap{"512m", "512m", 1024, 500}},
			{"traefik*", Cap{"512m", "512m", 1024, 500}},
			{"caddy*", Cap{"512m", "512m", 1024, 500}},
			{"envoy*", Cap{"1g", "1g", 1024, 500}},

			// Application servers
			{"helixagent*", Cap{"4g", "4g", 2048, 500}},
			{"helixllm*", Cap{"4g", "4g", 2048, 500}},
			{"helixqa*", Cap{"4g", "4g", 2048, 500}},
			{"helixmemory*", Cap{"4g", "4g", 2048, 500}},
			{"helix*", Cap{"3g", "3g", 2048, 500}},

			{"llm-verifier*", Cap{"4g", "4g", 2048, 500}},
			{"verifier*", Cap{"3g", "3g", 1024, 500}},

			// Build / CI
			{"*-builder*", Cap{"4g", "4g", 4096, 500}},
			{"*-ci-*", Cap{"3g", "3g", 4096, 500}},
			{"*-test*", Cap{"3g", "3g", 2048, 500}},
		},
	}
}

// parseSize converts compose-style size strings ("128", "512k", "2g") into
// raw bytes. Suffixes are case-insensitive: k/m/g/t for 1024-based units.
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	// Guard AFTER trimming: a whitespace-only input ("  ", "\t") passes an
	// s == "" check made before the trim, then s[len(s)-1] indexes s[-1] and
	// panics. Checking the trimmed value returns a clean "empty size" error
	// instead of crashing (§11.4.1 — a panic is a FAIL-bluff, not an error).
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'k':
		mult = 1 << 10
		s = s[:len(s)-1]
	case 'm':
		mult = 1 << 20
		s = s[:len(s)-1]
	case 'g':
		mult = 1 << 30
		s = s[:len(s)-1]
	case 't':
		mult = 1 << 40
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	// n * mult can silently wrap around uint64 (e.g. "20000000000g") and
	// return a small, wrong value with a nil error — a validation
	// false-negative of the same class as POL2-1. Reject overflow instead
	// of wrapping.
	if mult > 1 && n > math.MaxUint64/mult {
		return 0, fmt.Errorf("size %q overflows uint64", s)
	}
	return n * mult, nil
}

// yamlCap mirrors one `defaults:` or `patterns[]` entry in
// scripts/resource-policy/policy.yaml. Pids and OOMAdj are pointers so a
// field genuinely absent from the YAML (which inherits the top-level
// `defaults:` value per apply_caps.py's make_cap()) is distinguishable
// from an explicit `0`.
type yamlCap struct {
	Mem     string `yaml:"mem"`
	Memswap string `yaml:"memswap"`
	Pids    *int   `yaml:"pids"`
	OOMAdj  *int   `yaml:"oom_adj"`
}

// yamlPattern is one entry in policy.yaml's `patterns:` list.
type yamlPattern struct {
	yamlCap `yaml:",inline"`
	Match   string `yaml:"match"`
	Notes   string `yaml:"notes"`
}

// yamlPolicyFile is the top-level shape of policy.yaml.
type yamlPolicyFile struct {
	Defaults yamlCap       `yaml:"defaults"`
	Patterns []yamlPattern `yaml:"patterns"`
}

// resolve fills in a pattern-level yamlCap's Mem/Memswap/Pids/OOMAdj from the
// parsed defaults when the pattern omits them, mirroring apply_caps.py's
// make_cap(entry, default) fallback semantics. make_cap falls back ALL FOUR
// fields (`str(d.get("mem", default.mem))`, ... for memswap/pids/oom_adj) — an
// earlier version only inherited Pids/OOMAdj, so a pattern that legitimately
// omitted `mem:`/`memswap:` to inherit the top-level `defaults:` value (a
// make_cap-valid YAML shape) was scored as drift by [VerifyAgainstYAML] even
// though apply_caps.py produced an identical cap. An omitted mem/memswap
// surfaces as the zero-value empty string, treated here as "absent" → inherit
// the default, exactly as make_cap's d.get("mem", default.mem) does.
func (c yamlCap) resolve(defMem, defMemswap string, defPids, defOOM int) Cap {
	mem := c.Mem
	if mem == "" {
		mem = defMem
	}
	memswap := c.Memswap
	if memswap == "" {
		memswap = defMemswap
	}
	pids := defPids
	if c.Pids != nil {
		pids = *c.Pids
	}
	oomAdj := defOOM
	if c.OOMAdj != nil {
		oomAdj = *c.OOMAdj
	}
	return Cap{Mem: mem, Memswap: memswap, Pids: pids, OOMAdj: oomAdj}
}

// VerifyAgainstYAML parses the policy YAML at yamlPath (the canonical form
// lives at scripts/resource-policy/policy.yaml) and diffs it field-by-field
// against [Default]. It returns nil when the two representations agree,
// or a descriptive error naming the first drifted field otherwise.
//
// A pattern entry that omits any of `mem:` / `memswap:` / `pids:` / `oom_adj:`
// inherits the top-level `defaults:` value, exactly like apply_caps.py's
// make_cap(entry, default) (which falls back ALL FOUR fields) — a bare
// presence/absence diff would false-positive on every rule that relies on
// that fallback, so this mirrors that resolution rather than requiring every
// field to be spelled out explicitly.
func VerifyAgainstYAML(yamlPath string) error {
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", yamlPath, err)
	}
	var y yamlPolicyFile
	if err := yaml.Unmarshal(raw, &y); err != nil {
		return fmt.Errorf("parse %s: %w", yamlPath, err)
	}

	defPids := 1024
	if y.Defaults.Pids != nil {
		defPids = *y.Defaults.Pids
	}
	defOOM := 500
	if y.Defaults.OOMAdj != nil {
		defOOM = *y.Defaults.OOMAdj
	}
	yamlDefault := Cap{Mem: y.Defaults.Mem, Memswap: y.Defaults.Memswap, Pids: defPids, OOMAdj: defOOM}

	goPolicy := Default()
	if goPolicy.Default != yamlDefault {
		return fmt.Errorf("%s: default cap drift: policy.go has %+v, YAML has %+v",
			yamlPath, goPolicy.Default, yamlDefault)
	}

	if len(y.Patterns) != len(goPolicy.Rules) {
		return fmt.Errorf("%s: rule-count drift: policy.go has %d rules, YAML has %d patterns",
			yamlPath, len(goPolicy.Rules), len(y.Patterns))
	}

	for i, yp := range y.Patterns {
		gr := goPolicy.Rules[i]
		if gr.Match != yp.Match {
			return fmt.Errorf("%s: rule %d match drift: policy.go has %q, YAML has %q",
				yamlPath, i, gr.Match, yp.Match)
		}
		yCap := yp.yamlCap.resolve(y.Defaults.Mem, y.Defaults.Memswap, defPids, defOOM)
		if gr.Cap != yCap {
			return fmt.Errorf("%s: rule %d (%q) cap drift: policy.go has %+v, YAML has %+v",
				yamlPath, i, gr.Match, gr.Cap, yCap)
		}
	}
	return nil
}
