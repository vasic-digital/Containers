#!/usr/bin/env bash
# durable-run.test.sh — anti-bluff (CONST-XII) self-test for durable-run.sh.
# Proves a job launched via the helper KEEPS RUNNING under the user systemd
# manager (its own .service unit) after the launching shell exits, runs to
# completion producing real output, and that the OLD nohup approach does NOT
# get an independently-managed unit (the falsifiability contrast).
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Isolate artifacts so the test never collides with real runs.
export DURABLE_DIR="$(mktemp -d)"
# shellcheck source=/dev/null
source "${HERE}/durable-run.sh"

fail() { echo "FAIL: $*"; exit 1; }

# Honest skip when there is no usable systemd --user manager.
state="$(systemctl --user is-system-running 2>&1)"
if [ "$state" != "running" ] && [ "$state" != "degraded" ]; then
  echo "SKIP: no systemd --user manager (state=$state) — durability requires systemd-run --user"
  exit 0
fi

UNIT="durable-bashtest-$$-$RANDOM"
trap 'durable_stop "$UNIT"; rm -rf "$DURABLE_DIR"' EXIT

# Launch a sleeper that prints markers around a 6s sleep.
durable_launch_cmd "$UNIT" 'echo START_MARKER; sleep 6; echo DONE_MARKER' \
  || fail "durable_launch_cmd returned non-zero"

# The launching shell (systemd-run invocation) already returned; the job must
# nonetheless be active under the user manager.
active=0
for _ in $(seq 1 30); do
  if durable_is_active "$UNIT"; then active=1; break; fi
  sleep 0.1
done
[ "$active" = 1 ] || fail "unit not active after launch — job did not survive the launcher"

# Survival proof at the cgroup layer: independently-managed .service unit, not
# this shell's session scope.
pid="$(durable_main_pid "$UNIT")"
[ "${pid:-0}" -gt 0 ] || fail "no MainPID for unit"
job_cg="$(awk -F: '$1=="0"{print $3}' "/proc/${pid}/cgroup")"
self_cg="$(awk -F: '$1=="0"{print $3}' "/proc/$$/cgroup")"
echo "  job cgroup : $job_cg"
echo "  self cgroup: $self_cg"
case "$job_cg" in
  */"${UNIT}.service") : ;;  # good: own transient service unit
  *) fail "job cgroup is not an independently-managed .service unit: $job_cg" ;;
esac
[ "$job_cg" != "$self_cg" ] || fail "job shares the launcher cgroup — would be reaped with the session"

# Falsifiability contrast: the OLD nohup approach lands in the launcher's scope.
nohup bash -c 'sleep 6' >/dev/null 2>&1 &
np=$!
nohup_cg="$(awk -F: '$1=="0"{print $3}' "/proc/${np}/cgroup")"
kill "$np" 2>/dev/null || true
echo "  nohup cgroup: $nohup_cg"
case "$nohup_cg" in
  *.service) fail "nohup baseline unexpectedly independently managed ($nohup_cg) — test cannot tell durable from non-durable" ;;
  *) : ;;  # good: NOT a managed service unit
esac

# Completion + real output AFTER the launcher exited.
rc="$(durable_wait_sentinel "$UNIT" 25)" || fail "wait_sentinel timed out"
[ "$rc" = 0 ] || fail "durable job exit code = $rc, want 0"
log="$(durable_fetch_log "$UNIT")"
case "$log" in
  *START_MARKER*DONE_MARKER*) : ;;
  *) fail "durable job log missing markers (did not run to completion): $log" ;;
esac

echo "PASS: durable-run.sh — job survived launcher, ran to completion; nohup baseline is NOT independently managed"
