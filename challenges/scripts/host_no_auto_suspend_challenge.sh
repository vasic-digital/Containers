#!/usr/bin/env bash
# host_no_auto_suspend_challenge.sh — CONST-033 reproduction guard.
#
# Asserts the host this challenge runs on cannot be suspended /
# hibernated / put into hybrid-sleep by any user, session, DE, greeter,
# or cron job. Defence in depth: target masking + sleep.conf override
# + logind IdleAction override.
#
# Self-contained — no framework.sh dependency. Drop-in for any project's
# challenges/scripts/ directory.
#
# Pass criteria (4 assertions):
#   1. systemctl is-enabled sleep.target / suspend.target /
#      hibernate.target / hybrid-sleep.target ALL == "masked"
#   2. AllowSuspend=no found in /etc/systemd/sleep.conf or any
#      /etc/systemd/sleep.conf.d/*.conf drop-in
#   3. logind IdleAction == "ignore" (or unset, which defaults to ignore)
#   4. journalctl shows no "The system will suspend now" events since
#      the fix marker (/etc/systemd/sleep.conf.d/00-no-suspend.conf)
#      was written
#
# Exit:
#   0 = guard verified ACTIVE — every applicable assertion passed
#   1 = guard is genuinely BROKEN — systemd is live on this host, so all
#       four assertions are meaningful, and at least one of them failed
#   2 = COULD NOT DETERMINE / NOT APPLICABLE — this host is not running
#       systemd, so there is nothing here for these drop-ins to configure;
#       or an assertion's own instrument was unavailable and the assertion
#       could therefore neither pass nor fail. 2 is NEVER a pass: after a 2
#       the host is NOT known to be protected against suspend.
#
# Why the third value exists. Every assertion below interrogates systemd:
# `systemctl is-enabled`, systemd drop-in files, and systemd's journal. On a
# host that does not run systemd none of those instruments is measuring
# anything, and the previous two-valued contract reported that as exit 1 — the
# same verdict as a host whose guard had been dismantled. Those are opposite
# conditions and they demand opposite responses: one needs the guard repaired,
# the other needs a DIFFERENT power manager configured (elogind / acpid / a
# desktop environment's own policy), which this challenge does not inspect at
# all. Collapsing them into "FAIL" made the challenge a blind instrument
# reporting a confident verdict (§11.4.6).
#
# Two host facts this contract depends on, both measured rather than assumed:
#   * `systemctl mask` returns 0 on a host that merely HAS systemctl but was
#     not booted with systemd — masking is an offline symlink operation that
#     needs no running manager. So neither systemctl's presence on PATH nor a
#     0 from any of its offline verbs is evidence that a running manager
#     exists. `systemctl is-enabled` likewise answers from unit files on disk:
#     re-derive by running this challenge inside `unshare -rm` with a tmpfs
#     over /run/systemd — it answers "static", not an error.
#   * /run/systemd/system is the canonical sd_booted(3) probe for a live
#     manager. `systemctl is-system-running` is NOT a substitute: it exits
#     non-zero merely because the system is `degraded`, which is a false
#     negative for the question asked here. Re-derive:
#       systemctl is-system-running; echo "rc=$?"   # degraded -> rc=1
#       ls -d /run/systemd/system;   echo "rc=$?"   # exists   -> rc=0
#
# Contract guarded by:
#   bash challenges/scripts/host_no_auto_suspend_contract_challenge.sh

set -uo pipefail

PASS_COUNT=0
FAIL_COUNT=0
UNDET_COUNT=0
FAIL_DETAILS=()
UNDET_DETAILS=()

assert_pass() { echo "PASS: $*"; PASS_COUNT=$((PASS_COUNT + 1)); }
assert_fail() { echo "FAIL: $*"; FAIL_COUNT=$((FAIL_COUNT + 1)); FAIL_DETAILS+=("$*"); }
assert_undet() {
  echo "UNDETERMINED: $*"
  UNDET_COUNT=$((UNDET_COUNT + 1))
  UNDET_DETAILS+=("$*")
}

echo "=== host_no_auto_suspend_challenge ==="
echo

# --- capability detection -------------------------------------------------
# Runs BEFORE any assertion, because an assertion made with an instrument that
# cannot measure is not a weaker assertion — it is a fabricated one. Note in
# particular that assertion 3 would otherwise report "PASS: IdleAction=<unset>
# (safe)" on a host with no logind at all: the "unset defaults to ignore"
# reasoning is sound ONLY where logind is the thing making the decision.
if ! command -v systemctl >/dev/null 2>&1; then
  echo "COULD NOT DETERMINE: systemctl is not on PATH." >&2
  echo "  This host does not appear to run systemd, so the systemd drop-ins" >&2
  echo "  this challenge asserts on cannot apply to it. NOTHING was measured." >&2
  echo "  The host is NOT known to be protected against suspend — verify" >&2
  echo "  whatever power manager it does use (elogind / acpid / DE policy)." >&2
  exit 2
fi

if [[ ! -d /run/systemd/system ]]; then
  echo "COULD NOT DETERMINE: systemctl is present, but this host is not booted" >&2
  echo "  with systemd (/run/systemd/system does not exist). 'systemctl" >&2
  echo "  is-enabled' would answer from unit files on disk that no running" >&2
  echo "  manager reads, so a 'masked' answer here would prove nothing." >&2
  echo "  The host is NOT known to be protected against suspend." >&2
  exit 2
fi

# --- Test 1: sleep targets masked ---
echo "[1/4] sleep / suspend / hibernate / hybrid-sleep targets masked?"
unmasked=()
for tgt in sleep.target suspend.target hibernate.target hybrid-sleep.target; do
  state=$( { systemctl is-enabled "$tgt" 2>/dev/null || true; } | head -n1 | tr -d '[:space:]')
  [[ -z "$state" ]] && state="unknown"
  echo "    $tgt: $state"
  [[ "$state" != "masked" ]] && unmasked+=( "$tgt($state)" )
done
if [[ ${#unmasked[@]} -eq 0 ]]; then
  assert_pass "all 4 sleep targets masked"
else
  assert_fail "unmasked targets: ${unmasked[*]}"
fi

# --- Test 2: sleep.conf forbids suspend ---
echo "[2/4] AllowSuspend=no in sleep.conf or drop-in?"
if grep -shqE "^AllowSuspend[[:space:]]*=[[:space:]]*no" \
     /etc/systemd/sleep.conf /etc/systemd/sleep.conf.d/*.conf 2>/dev/null; then
  assert_pass "AllowSuspend=no present"
else
  assert_fail "AllowSuspend=no NOT found in sleep.conf or any drop-in"
fi

# --- Test 3: logind IdleAction=ignore ---
echo "[3/4] logind IdleAction safe?"
idle_action=$( { grep -shE "^IdleAction[[:space:]]*=" \
  /etc/systemd/logind.conf /etc/systemd/logind.conf.d/*.conf 2>/dev/null || true; } \
  | tail -n1 | cut -d= -f2 | tr -d '[:space:]')
idle_action=${idle_action:-"<unset>"}
echo "    logind IdleAction: $idle_action"
if [[ "$idle_action" == "ignore" ]] || [[ "$idle_action" == "<unset>" ]]; then
  assert_pass "IdleAction=$idle_action (safe)"
else
  assert_fail "IdleAction=$idle_action — could trigger suspend"
fi

# --- Test 4: no suspend events since fix ---
echo "[4/4] journal: any 'will suspend' broadcast since fix?"
fix_marker="/etc/systemd/sleep.conf.d/00-no-suspend.conf"

# Portable mtime. The previous spelling was
#   date -d "@$(stat -c %Y "$f")" -Iseconds || stat -c %y "$f"
# whose `||` branch is ALSO GNU-only, so there was no BSD path at all — only
# the appearance of one. BSD stat answers -f, GNU stat answers -c; BSD date
# formats an epoch with -r, GNU date with -d @.
#
# A CHAIN OF `||` IS NOT ENOUGH, and the obvious repair is itself broken.
# `stat -f %m "$f" 2>/dev/null || stat -c %Y "$f" 2>/dev/null` looks like a
# BSD-first dispatch, but on GNU coreutils `-f` means --file-system, so `%m`
# is parsed as a FILE operand: stat then writes a full filesystem report to
# STDOUT and exits 1. The `||` fires, the GNU spelling appends the real epoch,
# and the command substitution returns BOTH concatenated. Measured on GNU
# coreutils, `stat -f %m /etc/hostname` prints 7 lines of btrfs statistics and
# rc=1. Re-derive:
#   stat -f %m /etc/hostname; echo "rc=$?"
#   v=$(stat -f %m /etc/hostname 2>/dev/null || stat -c %Y /etc/hostname); echo "[$v]"
# So each spelling is run on its own and its OUTPUT is validated, not just its
# exit code. A host where neither spelling yields a bare epoch is UNDETERMINED,
# never a pass.
portable_mtime() {
  local m
  for m in "$(stat -c %Y "$1" 2>/dev/null)" "$(stat -f %m "$1" 2>/dev/null)"; do
    if [[ "$m" =~ ^[0-9]+$ ]]; then
      printf '%s\n' "$m"
      return 0
    fi
  done
  return 1
}
portable_iso() {
  local d
  for d in "$(date -d "@$1" -Iseconds 2>/dev/null)" \
           "$(date -r "$1" +%Y-%m-%dT%H:%M:%S%z 2>/dev/null)"; do
    # Same discipline as portable_mtime: GNU date reads -r as --reference=FILE
    # and BSD date rejects -d, so validate the SHAPE of what came back rather
    # than trusting an exit code.
    if [[ "$d" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2} ]]; then
      printf '%s\n' "$d"
      return 0
    fi
  done
  return 1
}

if [[ ! -f "$fix_marker" ]]; then
  # A live systemd host with no marker is a determinate failure: the guard was
  # never installed here. This is not a could-not-determine.
  assert_fail "fix marker $fix_marker missing — run install-host-suspend-guard.sh"
elif ! command -v journalctl >/dev/null 2>&1; then
  assert_undet "journalctl is not on PATH — cannot read the journal, so past" \
    "suspend events can be neither confirmed nor ruled out"
elif ! fix_mtime=$(portable_mtime "$fix_marker"); then
  assert_undet "neither 'stat -f %m' nor 'stat -c %Y' works on this host —" \
    "cannot anchor the journal window to the fix time"
elif ! fix_iso=$(portable_iso "$fix_mtime"); then
  assert_undet "neither 'date -d @<epoch>' nor 'date -r <epoch>' works on this" \
    "host — cannot anchor the journal window to the fix time"
else
  echo "    fix applied at: $fix_iso"

  # journalctl's own failure must not be laundered into "0 events found" — but
  # the journal MUST NOT be buffered to get that. The window here is open-ended
  # (it starts when the guard was installed, which can be months ago): measured
  # on the development host, `journalctl --since` over that window had already
  # streamed more than 190 MB after 100 seconds and was still going. Capturing
  # it with `$(...)` puts the whole thing in one shell variable and the shell
  # dies — observed as rc=139, SIGSEGV, on a run that used to exit 0.
  #
  # So the pipeline is preserved, exactly as it always was, and grep counts in
  # constant memory. journalctl's exit status is recovered from PIPESTATUS[0],
  # which is only readable immediately after the pipeline and only when the
  # pipeline is NOT itself inside a command substitution — hence the small
  # scratch file rather than a nested `$( )`. No mktemp: it is one more tool to
  # depend on, and $$ plus a trap is enough.
  scratch="${TMPDIR:-/tmp}/host_no_auto_suspend_challenge.$$"
  trap 'rm -f "$scratch"' EXIT
  journalctl --since "$fix_iso" --no-pager 2>/dev/null \
    | { grep -c "The system will suspend now" || true; } > "$scratch"
  journal_rc=${PIPESTATUS[0]}

  if [[ "$journal_rc" -ne 0 ]]; then
    assert_undet "journalctl --since '$fix_iso' exited $journal_rc — the journal" \
      "could not be read, so suspend events can be neither confirmed nor ruled out"
  else
    count=$(head -n1 "$scratch" | tr -dc '0-9')
    count=${count:-0}
    echo "    'will suspend' broadcasts since fix: $count"
    if [[ "$count" -eq 0 ]]; then
      assert_pass "no suspend events since fix at $fix_iso"
    else
      assert_fail "$count suspend events since fix — masking didn't take"
    fi
  fi
  rm -f "$scratch"
fi

echo
echo "=== summary: $PASS_COUNT pass, $FAIL_COUNT fail, $UNDET_COUNT undetermined ==="

if [[ $FAIL_COUNT -gt 0 ]]; then
  # A real finding outranks an unmeasurable one: something that WAS measured
  # came back wrong, and that is actionable now.
  exit 1
fi

if [[ $UNDET_COUNT -gt 0 ]]; then
  echo "COULD NOT DETERMINE: $UNDET_COUNT assertion(s) had no working instrument." >&2
  for d in "${UNDET_DETAILS[@]}"; do echo "  - $d" >&2; done
  echo "  THIS IS NOT A PASS. The guard is not known to be active on this host." >&2
  exit 2
fi

exit 0
