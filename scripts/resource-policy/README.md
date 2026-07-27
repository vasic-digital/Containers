# resource-policy

Container resource caps for the consuming project. Prevents the user GUI
session from being SIGKILL'd by memory pressure when multiple stacks are
running concurrently.

See `../../docs/RESOURCE_LIMITS.md` for the full design rationale.

## Files

| File | Purpose |
|---|---|
| `policy.yaml` | The cap rules, used by `apply_caps.py`. Single source of truth #1. |
| `apply_caps.py` | Bulk-applies caps to every user-owned compose file in the repo. Idempotent. |
| `test_caps.py` | 20 tests covering policy matching, file coverage, placement, budget, idempotency. |
| `apply-report.md` | Generated report from the last `apply_caps.py` run. |

The Go counterpart of `policy.yaml` lives at
`pkg/policy/policy.go` (relative to this module's own root). The two should always agree.

## Quick start

```sh
# Preview changes
./apply_caps.py --dry-run

# Apply
./apply_caps.py

# Test
python3 test_caps.py
```

## Adding a new container

Add the service to its compose file and run `apply_caps.py`. If the
service name matches an existing pattern in `policy.yaml`, you're done.
Otherwise, add a more-specific rule near the top of `patterns:` in
`policy.yaml`, **and** add the equivalent entry in
`pkg/policy/policy.go::Default()` (relative to this module's own root).

## What it does NOT touch

Third-party submodules (`MCP/submodules/`, `external/`, third-party
`cli_agents/*`), vendored deps, docs/examples. See
`apply_caps.py::SKIP_PATH_FRAGMENTS` for the full list.

## Without sudo

Everything in this directory works as the regular user — no privileged
commands. Caps are enforced by the kernel via cgroup v2 user delegation
(systemd-user-managed `memory`, `pids`, `cpu` controllers).
