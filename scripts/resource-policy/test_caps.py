#!/usr/bin/env python3
"""Comprehensive tests for the container resource cap policy.

Run all tests::

    python3 -m pytest test_caps.py -v

Or, without pytest installed::

    python3 test_caps.py

Test groups:

* ``TestPolicyMatching`` — service-name patterns map to the expected caps.
* ``TestComposeFiles`` — every user-owned compose file has caps on every
  service, with values inside sane ranges.
* ``TestTotalBudget`` — sum of caps per profile / file stays within the
  host's safety budget so the user GUI session can never be starved.
* ``TestPlacement`` — caps are positioned right after the identity keys,
  not after trailing comments (regression test for an earlier bug).
* ``TestSmoke`` — the policy applies cleanly on a synthetic compose
  fixture and is idempotent on a second run.
"""
from __future__ import annotations

import os
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Any

from ruamel.yaml import YAML

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent.parent  # HelixAgent/
POLICY_PATH = HERE / "policy.yaml"
APPLY_SCRIPT = HERE / "apply_caps.py"

# Inject the script dir on sys.path so we can import its helpers.
sys.path.insert(0, str(HERE))
import apply_caps as ac  # noqa: E402

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
SIZE_RE = re.compile(r"^(\d+)([kmgtKMGT])?$")


def parse_size_to_bytes(value: str) -> int:
    """Parse `2g` / `512m` / `1024` into bytes."""
    if isinstance(value, int):
        return value
    s = str(value).strip().lower()
    m = SIZE_RE.match(s)
    if not m:
        raise ValueError(f"unparseable size: {value!r}")
    n = int(m.group(1))
    unit = m.group(2)
    mul = {None: 1, "k": 1024, "m": 1024**2, "g": 1024**3, "t": 1024**4}[unit]
    return n * mul


def collect_user_compose_files() -> list[Path]:
    """Walk the project using the same skip rules as ``apply_caps.py``."""
    return ac.find_compose_files(ROOT)


def load_compose(path: Path) -> dict[str, Any]:
    """Load a compose doc, tolerating pre-existing duplicate keys in the
    file (which are upstream bugs we don't auto-fix)."""
    yaml = YAML(typ="safe")
    yaml.allow_duplicate_keys = True
    with path.open() as fh:
        return yaml.load(fh) or {}


# ---------------------------------------------------------------------------
# Policy matching
# ---------------------------------------------------------------------------
class TestPolicyMatching(unittest.TestCase):
    def setUp(self) -> None:
        self.policy = ac.load_policy(POLICY_PATH)

    def test_default_cap(self) -> None:
        cap = self.policy.cap_for("totally-unknown-service-name")
        assert cap.mem == "2g"
        assert cap.pids == 1024
        assert cap.oom_adj == 500

    def test_postgres_pattern(self) -> None:
        cap = self.policy.cap_for("postgres")
        assert cap.mem == "4g"
        cap = self.policy.cap_for("postgres-test")
        assert cap.mem == "4g"
        cap = self.policy.cap_for("helixagent-postgres")
        assert cap.mem == "4g"

    def test_redis_pattern(self) -> None:
        cap = self.policy.cap_for("redis")
        assert cap.mem == "1g"
        cap = self.policy.cap_for("redis-test")
        assert cap.mem == "1g"

    def test_mcp_generic_pattern(self) -> None:
        cap = self.policy.cap_for("mcp-fetch")
        assert cap.mem == "1g"
        cap = self.policy.cap_for("mcp-some-new-server")
        assert cap.mem == "1g"

    def test_mcp_browser_specific_takes_priority(self) -> None:
        for name in ("mcp-puppeteer", "mcp-playwright", "mcp-browserbase"):
            cap = self.policy.cap_for(name)
            assert cap.mem == "3g", name
            assert cap.pids == 4096, name

    def test_llm_pattern(self) -> None:
        cap = self.policy.cap_for("ollama")
        assert cap.mem == "12g"
        assert cap.pids == 2048
        cap = self.policy.cap_for("vllm-runner")
        assert cap.mem == "12g"

    def test_helix_app_servers(self) -> None:
        cap = self.policy.cap_for("helixagent")
        assert cap.mem == "4g"
        assert cap.pids == 2048
        cap = self.policy.cap_for("helixllm-server")
        assert cap.mem == "4g"

    def test_case_insensitive(self) -> None:
        cap = self.policy.cap_for("POSTGRES")
        assert cap.mem == "4g"
        cap = self.policy.cap_for("Redis-Cache")
        assert cap.mem == "1g"

    def test_oom_adj_always_positive(self) -> None:
        for name in ("postgres", "redis", "mcp-fetch", "ollama", "elasticsearch",
                     "unknown-svc"):
            cap = self.policy.cap_for(name)
            assert cap.oom_adj > 0, f"{name} oom_adj must be >0 to protect user manager"

    def test_memswap_never_smaller_than_mem(self) -> None:
        """memswap is the TOTAL memory + swap budget; in cgroup v2 semantics
        memswap_limit must be >= mem_limit."""
        for name in ("postgres", "redis", "mcp-fetch", "ollama", "elasticsearch",
                     "unknown-svc", "neo4j", "sonarqube"):
            cap = self.policy.cap_for(name)
            assert parse_size_to_bytes(cap.memswap) >= parse_size_to_bytes(cap.mem), f"{name}: memswap {cap.memswap} < mem {cap.mem}"


# ---------------------------------------------------------------------------
# Compose-file coverage
# ---------------------------------------------------------------------------
class TestComposeFiles(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.files = collect_user_compose_files()
        if not cls.files:
            raise unittest.SkipTest("no compose files found")

    def test_files_discovered(self) -> None:
        assert len(self.files) > 30, "expected at least 30 user-owned compose files"

    def test_every_service_has_all_caps(self) -> None:
        missing: list[tuple[str, str, list[str]]] = []
        for path in self.files:
            doc = load_compose(path)
            services = doc.get("services") or {}
            if not isinstance(services, dict):
                continue
            for name, svc in services.items():
                if not isinstance(svc, dict):
                    continue
                # PyYAML safe-load resolves `<<:` merges, so an inherited
                # cap from an anchor counts for this test.
                lacks = [k for k in ac.CAP_KEYS if k not in svc]
                if lacks:
                    missing.append((str(path.relative_to(ROOT)), str(name), lacks))
        if missing:
            sample = "\n".join(f"  {p}::{s}  missing={l}" for p, s, l in missing[:20])
            self.fail(f"{len(missing)} service(s) missing caps. Sample:\n{sample}")

    def test_cap_values_are_sane(self) -> None:
        """Memory caps must be in [128m, 32g], pids in [128, 8192],
        oom_score_adj in [100, 1000]."""
        bad: list[str] = []
        for path in self.files:
            doc = load_compose(path)
            for name, svc in (doc.get("services") or {}).items():
                if not isinstance(svc, dict):
                    continue
                mem = svc.get("mem_limit")
                if mem is not None:
                    n = parse_size_to_bytes(str(mem))
                    if not (128 * 1024**2 <= n <= 32 * 1024**3):
                        bad.append(f"{path.name}::{name} mem_limit={mem}")
                pids = svc.get("pids_limit")
                if pids is not None and not (128 <= int(pids) <= 8192):
                    bad.append(f"{path.name}::{name} pids_limit={pids}")
                oom = svc.get("oom_score_adj")
                if oom is not None and not (100 <= int(oom) <= 1000):
                    bad.append(f"{path.name}::{name} oom_score_adj={oom}")
        if bad:
            self.fail("Out-of-range caps:\n  " + "\n  ".join(bad[:20]))

    def test_memswap_at_least_mem(self) -> None:
        bad: list[str] = []
        for path in self.files:
            doc = load_compose(path)
            for name, svc in (doc.get("services") or {}).items():
                if not isinstance(svc, dict):
                    continue
                mem = svc.get("mem_limit")
                memswap = svc.get("memswap_limit")
                if mem is None or memswap is None:
                    continue
                if parse_size_to_bytes(str(memswap)) < parse_size_to_bytes(str(mem)):
                    bad.append(f"{path.name}::{name} memswap={memswap} < mem={mem}")
        if bad:
            self.fail("memswap < mem in some services:\n  " + "\n  ".join(bad[:20]))

    def test_oom_score_adj_protects_user_manager(self) -> None:
        """Every container must have oom_score_adj > 0 so the kernel/oomd
        prefers killing the container over the user systemd manager."""
        bad: list[str] = []
        for path in self.files:
            doc = load_compose(path)
            for name, svc in (doc.get("services") or {}).items():
                if not isinstance(svc, dict):
                    continue
                oom = svc.get("oom_score_adj")
                if oom is None:
                    continue
                if int(oom) <= 0:
                    bad.append(f"{path.name}::{name} oom_score_adj={oom}")
        if bad:
            self.fail("Containers not preferred OOM victims:\n  " + "\n  ".join(bad[:20]))


# ---------------------------------------------------------------------------
# Total budget per file (so a single profile can't exhaust the host)
# ---------------------------------------------------------------------------
class TestTotalBudget(unittest.TestCase):
    HOST_TOTAL_GIB = 62
    SAFETY_FREE_GIB = 12          # GUI + kernel + AI agents
    BUDGET_GIB = HOST_TOTAL_GIB - SAFETY_FREE_GIB  # 50 GiB

    def setUp(self) -> None:
        self.files = collect_user_compose_files()

    # Files that intentionally define many profiles where users only ever
    # `up` a subset at a time. We don't enforce the host budget for these.
    PROFILE_HEAVY_FILES = {
        "docker-compose.yml",                  # kitchen-sink: select via profile
        "docker-compose.mcp-full.yml",         # 80+ MCPs, profiles per tier
        "docker-compose.protocols.yml",        # all protocol stacks
        "docker-compose.bigdata.yml",          # mutually-exclusive engines
        "docker-compose.production.yml",
        "docker-compose.test-full.yml",
        "docker-compose.mcp-all-servers.yml",
        "docker-compose.mcp-servers.yml",
    }

    def test_single_file_fits_budget(self) -> None:
        """If you `up` an entire compose file (no profile filter), its
        total caps shouldn't exceed the host budget. Exception: files
        designed for profile-based selection are exempt — see
        ``PROFILE_HEAVY_FILES``. The user-facing wrapper ``compose-up``
        adds its own 16G systemd scope as a second backstop."""
        offenders: list[str] = []
        for path in self.files:
            if path.name in self.PROFILE_HEAVY_FILES:
                continue
            doc = load_compose(path)
            total = 0
            for _name, svc in (doc.get("services") or {}).items():
                if not isinstance(svc, dict):
                    continue
                mem = svc.get("mem_limit")
                if mem:
                    total += parse_size_to_bytes(str(mem))
            total_gib = total / 1024**3
            if total_gib > self.BUDGET_GIB:
                offenders.append(f"{path.name}: {total_gib:.1f} GiB")
        if offenders:
            self.fail(f"Non-profile compose files exceed {self.BUDGET_GIB} GiB:\n  "
                      + "\n  ".join(offenders)
                      + "\n  → either lower per-service caps, split into "
                      + "profiles, or add the file to PROFILE_HEAVY_FILES.")

    def test_profile_heavy_files_exist(self) -> None:
        """Sanity: at least one of the profile-heavy files exists in the
        repo — the exemption list shouldn't be a list of phantom names."""
        names = {p.name for p in self.files}
        seen = self.PROFILE_HEAVY_FILES & names
        assert len(seen) > 0, "PROFILE_HEAVY_FILES is empty in this repo — the exemption " "list is stale or no longer needed."


# ---------------------------------------------------------------------------
# Placement regression test
# ---------------------------------------------------------------------------
class TestPlacement(unittest.TestCase):
    """Caps must be inserted *near the top* of the service block, not at
    the end where trailing comments can corrupt the diff."""

    def test_caps_appear_near_top_of_service(self) -> None:
        """For each service the FIRST cap field this script wrote
        (oom_score_adj is unique to us — no upstream sets it) should
        appear within a small number of lines of the service header.
        This catches the regression where caps landed after a trailing
        comment of the previous service."""
        bad: list[str] = []
        for path in collect_user_compose_files():
            text = path.read_text().splitlines()
            svc_re = re.compile(r"^  ([A-Za-z0-9_-]+):\s*$")
            # oom_score_adj is the most reliable signal — no compose author
            # writes it by hand, so wherever it lands is where our script
            # placed caps.
            cap_re = re.compile(r"^\s+oom_score_adj:")
            current_svc = None
            current_line = -1
            for i, line in enumerate(text):
                m = svc_re.match(line)
                if m and not line.lstrip().startswith("#"):
                    current_svc = m.group(1)
                    current_line = i
                    continue
                if cap_re.match(line) and current_svc is not None:
                    gap = i - current_line
                    if gap > 25:
                        bad.append(f"{path.name}::{current_svc} oom_score_adj is {gap} lines below header")
                    current_svc = None
        if bad:
            self.fail("Caps placed far from service header:\n  " + "\n  ".join(bad[:20]))


# ---------------------------------------------------------------------------
# Smoke test on a synthetic compose fixture
# ---------------------------------------------------------------------------
SAMPLE_COMPOSE = """\
services:
  postgres:
    image: postgres:15
    container_name: t-postgres
    environment:
      POSTGRES_DB: t
  redis:
    image: redis:7
  some-mcp:
    image: mcp:latest
    container_name: t-mcp-fetch
"""


class TestSmoke(unittest.TestCase):
    def test_apply_then_validate_is_idempotent(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            tdp = Path(td)
            f = tdp / "docker-compose.yml"
            f.write_text(SAMPLE_COMPOSE)
            env = {**os.environ, "PYTHONPATH": str(HERE)}
            r = subprocess.run(
                [sys.executable, str(APPLY_SCRIPT), "--root", td],
                capture_output=True, text=True, env=env)
            assert r.returncode == 0, r.stderr
            doc = load_compose(f)
            for name in ("postgres", "redis", "some-mcp"):
                svc = doc["services"][name]
                for k in ac.CAP_KEYS:
                    assert k in svc, f"{name} missing {k} after first apply"
            # Second run must be no-op
            r2 = subprocess.run(
                [sys.executable, str(APPLY_SCRIPT), "--root", td],
                capture_output=True, text=True, env=env)
            assert r2.returncode == 0, r2.stderr
            doc2 = load_compose(f)
            assert doc == doc2, "second run was not idempotent"

    def test_pattern_matches_correct_pid_limits(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            tdp = Path(td)
            f = tdp / "docker-compose.yml"
            f.write_text(SAMPLE_COMPOSE)
            subprocess.run(
                [sys.executable, str(APPLY_SCRIPT), "--root", td],
                capture_output=True, text=True,
                env={**os.environ, "PYTHONPATH": str(HERE)})
            doc = load_compose(f)
            assert doc["services"]["postgres"]["mem_limit"] == "4g"
            assert doc["services"]["redis"]["mem_limit"] == "1g"
            # `some-mcp` should match `mcp-*` pattern → 1g, but the service
            # is named `some-mcp` (does NOT match `mcp-*`). It falls to
            # default 2g. This validates left-anchored pattern matching.
            assert doc["services"]["some-mcp"]["mem_limit"] == "2g"


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
if __name__ == "__main__":
    unittest.main(verbosity=2)
