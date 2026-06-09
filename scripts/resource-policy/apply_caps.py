#!/usr/bin/env python3
"""apply_caps.py — bulk-apply container resource caps to compose files.

Reads ``policy.yaml`` (next to this script) and walks the project tree
applying ``mem_limit``, ``memswap_limit``, ``pids_limit`` and
``oom_score_adj`` to every service in every user-owned compose file.

Design decisions:

* **ruamel.yaml** preserves comments, anchors, and key order. Every other YAML
  library would trash hand-written compose files.
* **First-match-wins** policy: most specific patterns at the top of
  ``policy.yaml``. Avoids surprises when a pattern accidentally catches
  unrelated services.
* **Idempotent**: re-running on already-capped files is a no-op (existing
  cap fields are preserved unless ``--force-rewrite`` is used).
* **Anchor-aware**: when a service uses ``<<: *anchor`` and the anchor block
  has caps, the service inherits them — we won't add duplicates.
* **Skip-list driven**: third-party submodules (modelcontextprotocol,
  microsoft, openhands, etc.) are excluded by path so we never modify
  upstreams we don't own.

Usage::

    apply_caps.py [--root <dir>] [--policy <yaml>] [--dry-run]
                  [--only-file <glob>] [--exclude <glob>]
                  [--report <md>]

Exit codes::

    0  — at least one file was edited (or would be in --dry-run)
    1  — argument error
    2  — no compose files matched
    3  — policy error
"""
from __future__ import annotations

import argparse
import fnmatch
import os
import sys
from dataclasses import dataclass
from pathlib import Path

try:
    from ruamel.yaml import YAML
    from ruamel.yaml.comments import CommentedMap, CommentedSeq
except ImportError as exc:  # pragma: no cover
    print(f"ruamel.yaml is required: {exc}", file=sys.stderr)
    sys.exit(3)


# ---------------------------------------------------------------------------
# Skip rules — paths whose compose files we must NOT modify.
# ---------------------------------------------------------------------------
# Universal skip patterns — fragments true for every consuming project,
# regardless of repo layout. Per CONST-051(B) this submodule MUST NOT
# enumerate consuming-project-internal paths in source; project-specific
# skip fragments come from `--skip-paths-file` / `RESOURCE_POLICY_SKIP_PATHS_FILE`
# at runtime, supplied by the consuming project (see the example file shipped
# alongside this script at `resource-policy-skip-paths.example.list`).
SKIP_PATH_FRAGMENTS_DEFAULT: tuple[str, ...] = (
    "/.git/",
    "/node_modules/",
    "/vendor/",
    "/external/",
    # Our own backup directories
    "/.container-caps-backup-",
    # Documentation directories that contain example compose files
    "/docs/research/",
    "/docs/specs/",
    "/docs/examples/",
)

# Mutable, set at startup from defaults + --skip-paths-file + --exclude.
SKIP_PATH_FRAGMENTS: tuple[str, ...] = SKIP_PATH_FRAGMENTS_DEFAULT


def load_skip_paths_file(path: Path) -> tuple[str, ...]:
    """Load extra skip-path fragments from a one-per-line file.

    Per CONST-051(B): the consuming project supplies its own project-
    specific skip fragments via this file so this submodule's source
    stays decoupled from any specific consumer's directory layout.

    Format:
      - One fragment per line.
      - Leading/trailing whitespace stripped.
      - Lines starting with `#` (after stripping) are comments.
      - Blank lines ignored.

    The file MUST exist; missing-file is a hard error (the operator
    invoked us with --skip-paths-file so they expect it).
    """
    if not path.is_file():
        raise FileNotFoundError(f"--skip-paths-file not found: {path}")
    out: list[str] = []
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        out.append(line)
    return tuple(out)

COMPOSE_GLOBS: tuple[str, ...] = (
    "docker-compose*.yml",
    "docker-compose*.yaml",
    "compose.yml",
    "compose.yaml",
    "podman-compose*.yml",
)

# Cap fields, in the canonical order we emit them.
CAP_KEYS: tuple[str, ...] = ("mem_limit", "memswap_limit", "pids_limit", "oom_score_adj")


# ---------------------------------------------------------------------------
# Policy loading
# ---------------------------------------------------------------------------
@dataclass
class Cap:
    mem: str
    memswap: str
    pids: int
    oom_adj: int

    def to_dict(self) -> dict:
        return {
            "mem_limit": self.mem,
            "memswap_limit": self.memswap,
            "pids_limit": self.pids,
            "oom_score_adj": self.oom_adj,
        }


@dataclass
class Policy:
    default: Cap
    rules: list[tuple[str, Cap]]

    def cap_for(self, service_name: str) -> Cap:
        name = service_name.lower()
        for pattern, cap in self.rules:
            if fnmatch.fnmatchcase(name, pattern.lower()):
                return cap
        return self.default


def load_policy(path: Path) -> Policy:
    yaml = YAML(typ="safe")
    data = yaml.load(path.read_text())
    if not isinstance(data, dict) or "defaults" not in data or "patterns" not in data:
        raise ValueError(f"{path}: missing 'defaults' / 'patterns' top-level keys")

    def make_cap(d: dict, fallback: Cap | None = None) -> Cap:
        return Cap(
            mem=str(d.get("mem", fallback.mem if fallback else "")),
            memswap=str(d.get("memswap", fallback.memswap if fallback else "")),
            pids=int(d.get("pids", fallback.pids if fallback else 1024)),
            oom_adj=int(d.get("oom_adj", fallback.oom_adj if fallback else 500)),
        )

    default = make_cap(data["defaults"])
    rules: list[tuple[str, Cap]] = []
    for entry in data["patterns"]:
        if not isinstance(entry, dict) or "match" not in entry:
            raise ValueError(f"{path}: each pattern needs 'match'")
        rules.append((entry["match"], make_cap(entry, default)))
    return Policy(default=default, rules=rules)


# ---------------------------------------------------------------------------
# Compose file walking
# ---------------------------------------------------------------------------
def is_skipped(path: Path) -> bool:
    s = "/" + str(path).replace(os.sep, "/")
    return any(frag in s for frag in SKIP_PATH_FRAGMENTS)


def find_compose_files(root: Path) -> list[Path]:
    out: list[Path] = []
    for dirpath, dirnames, filenames in os.walk(root):
        # prune skipped subtrees early
        keep_dirs = []
        for d in dirnames:
            full = Path(dirpath) / d
            if is_skipped(full):
                continue
            keep_dirs.append(d)
        dirnames[:] = keep_dirs
        for fn in filenames:
            for pat in COMPOSE_GLOBS:
                if fnmatch.fnmatchcase(fn, pat):
                    p = Path(dirpath) / fn
                    if not is_skipped(p):
                        out.append(p)
                    break
    return sorted(out)


# ---------------------------------------------------------------------------
# Cap insertion
# ---------------------------------------------------------------------------
def existing_cap_keys(svc: CommentedMap) -> set[str]:
    return {k for k in CAP_KEYS if k in svc}


def merged_anchor_caps(svc: CommentedMap) -> set[str]:
    """Return the cap keys this service inherits from anchors via ``<<:``.

    ruamel.yaml in round-trip mode stores merge keys in ``svc.merge`` as a
    list of ``(merge_index, anchor_node)`` tuples. The anchor node is the
    referenced CommentedMap. Inherited cap keys count as "already set" so
    we don't write redundant overrides.
    """
    caps: set[str] = set()
    merge = getattr(svc, "merge", None)
    if not merge:
        return caps
    # ruamel.yaml exposes `merge` as a MergeValue iterable of CommentedMaps,
    # but older versions used a list of (index, target) tuples. Handle both.
    for entry in merge:
        target: object = entry
        if isinstance(entry, tuple) and len(entry) >= 2:
            target = entry[1]
        if isinstance(target, CommentedMap):
            caps |= existing_cap_keys(target)
    return caps


def add_caps_to_service(
    svc: CommentedMap,
    cap: Cap,
    *,
    force: bool = False,
) -> list[str]:
    """Add cap keys to a service mapping. Returns list of keys added."""
    if not isinstance(svc, CommentedMap):
        return []
    have = existing_cap_keys(svc) | (set() if force else merged_anchor_caps(svc))
    added: list[str] = []
    cap_dict = cap.to_dict()
    # Insert caps near the top of the service block (right after the
    # identity/build keys) rather than at the end. This avoids visual
    # corruption when the previous service had a trailing block comment.
    insert_after = None
    for key in ("container_name", "image", "build", "<<"):
        if key in svc:
            insert_after = key
            break
    if insert_after is None:
        # Fallback: prefer a stable later key if no early one exists.
        for key in ("hostname", "command", "entrypoint", "user"):
            if key in svc:
                insert_after = key
                break
    pos = list(svc.keys()).index(insert_after) + 1 if insert_after else 0
    for key in CAP_KEYS:
        if key in have and not force:
            continue
        if key in svc:
            if force:
                svc[key] = cap_dict[key]
                added.append(key + " (forced)")
            # else: already present and not forcing, skip
        else:
            svc.insert(pos, key, cap_dict[key])
            pos += 1
            added.append(key)
    return added


def process_compose(
    path: Path,
    policy: Policy,
    *,
    dry_run: bool,
    force: bool,
) -> tuple[int, list[str]]:
    """Edit one compose file. Returns (services_changed, log_lines)."""
    yaml = YAML(typ="rt")
    yaml.preserve_quotes = True
    yaml.allow_duplicate_keys = True   # tolerate upstream bugs
    yaml.width = 4096
    yaml.indent(mapping=2, sequence=4, offset=2)
    try:
        with path.open() as fh:
            doc = yaml.load(fh)
    except Exception as exc:
        return 0, [f"  ERR  parse: {exc}"]
    if not isinstance(doc, CommentedMap):
        return 0, ["  SKIP not a mapping"]

    log: list[str] = []
    n_changed = 0

    # NOTE: We deliberately do NOT mutate top-level YAML anchors (e.g.
    # ``x-common: &common-config``). Doing so risks corrupting non-service
    # anchors like ``x-healthcheck`` and produces formatting glitches.
    # Instead every service gets its own explicit cap fields. Verbose but
    # safe, and `<<: *common-config` continues to work normally.

    services = doc.get("services")
    if not isinstance(services, CommentedMap):
        if n_changed and not dry_run:
            with path.open("w") as fh:
                yaml.dump(doc, fh)
        return n_changed, [*log, "  (no services section)"]

    for svc_name, svc in services.items():
        if not isinstance(svc, CommentedMap):
            continue
        cap = policy.cap_for(str(svc_name))
        added = add_caps_to_service(svc, cap, force=force)
        if added:
            n_changed += 1
            log.append(f"  {svc_name}  +{','.join(added)}  ({cap.mem}/{cap.pids}p)")

    if n_changed and not dry_run:
        with path.open("w") as fh:
            yaml.dump(doc, fh)
    return n_changed, log


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
def main(argv: list[str]) -> int:
    here = Path(__file__).resolve().parent
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--root", default=os.getcwd(), type=Path,
                        help="project root to walk (default: cwd)")
    parser.add_argument("--policy", default=here / "policy.yaml", type=Path,
                        help="policy YAML to use (default: alongside script)")
    parser.add_argument("--dry-run", action="store_true",
                        help="preview changes without writing")
    parser.add_argument("--only-file", default=None,
                        help="only process files matching this glob")
    parser.add_argument("--exclude", default=None,
                        help="additional path fragment to skip (single)")
    parser.add_argument(
        "--skip-paths-file",
        default=os.environ.get("RESOURCE_POLICY_SKIP_PATHS_FILE"),
        type=Path,
        help=(
            "file with project-specific skip-path fragments, one per line. "
            "Comments (#) and blank lines ignored. The consuming project "
            "supplies this file so apply_caps stays decoupled from any "
            "specific consumer's directory layout (CONST-051(B)). Falls "
            "back to RESOURCE_POLICY_SKIP_PATHS_FILE env var."
        ),
    )
    parser.add_argument("--force-rewrite", action="store_true",
                        help="overwrite existing cap fields")
    parser.add_argument("--report", default=None, type=Path,
                        help="write a markdown change report to this file")
    args = parser.parse_args(argv)

    if not args.policy.is_file():
        print(f"policy not found: {args.policy}", file=sys.stderr)
        return 3
    try:
        policy = load_policy(args.policy)
    except Exception as exc:
        print(f"policy error: {exc}", file=sys.stderr)
        return 3

    # Build the effective skip-paths set: defaults + project-file + --exclude.
    global SKIP_PATH_FRAGMENTS
    effective_skip: tuple[str, ...] = SKIP_PATH_FRAGMENTS_DEFAULT
    if args.skip_paths_file:
        try:
            project_skips = load_skip_paths_file(args.skip_paths_file)
        except FileNotFoundError as exc:
            print(f"skip-paths-file error: {exc}", file=sys.stderr)
            return 1
        if project_skips:
            print(f"loaded {len(project_skips)} skip-path fragment(s) from {args.skip_paths_file}")
            effective_skip = effective_skip + project_skips
    if args.exclude:
        effective_skip = (*effective_skip, args.exclude)
    SKIP_PATH_FRAGMENTS = effective_skip

    files = find_compose_files(args.root.resolve())
    if args.only_file:
        files = [f for f in files if fnmatch.fnmatchcase(f.name, args.only_file)]

    if not files:
        print("no compose files matched", file=sys.stderr)
        return 2

    total_files = 0
    total_services = 0
    report_lines: list[str] = []
    for path in files:
        rel = path.relative_to(args.root.resolve())
        n, log = process_compose(path, policy, dry_run=args.dry_run, force=args.force_rewrite)
        if n:
            total_files += 1
            total_services += n
            print(f"[{n:>3} svc] {rel}")
            for line in log:
                print(line)
            report_lines.append(f"### `{rel}` — {n} service(s) updated\n")
            for line in log:
                report_lines.append(f"- {line.strip()}")
            report_lines.append("")
        elif log:
            print(f"[ - ] {rel}")
            for line in log:
                print(line)

    summary = (
        f"\nTotal: {total_services} service(s) across {total_files} file(s)"
        + (" (DRY RUN, no writes)" if args.dry_run else "")
    )
    print(summary)

    if args.report:
        args.report.write_text(
            "# Container Cap Application Report\n\n"
            f"Policy: `{args.policy}`\n\n"
            f"{summary.strip()}\n\n"
            + "\n".join(report_lines)
            + "\n"
        )
        print(f"report written to {args.report}")
    return 0 if total_services else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
