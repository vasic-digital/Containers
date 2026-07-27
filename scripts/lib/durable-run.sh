#!/usr/bin/env bash
# scripts/lib/durable-run.sh — DURABLE execution helper (shell form of
# pkg/remoteexec). Source this from any script that must launch a long job that
# SURVIVES the SSH/login session ending.
#
# Why: a remote host's systemd-logind reaps ALL of a user's processes when its
# last session closes (KillUserProcesses=yes on many hosts). tmux/nohup/setsid
# all live in the login session's `session-<n>.scope` cgroup and die with it.
# The fix is to run the job as its OWN transient user .service unit, owned by
# the user systemd manager (with linger enabled), so it outlives the session.
#
# This is the canonical SHELL implementation, shared by every consuming repo via
# the containers submodule (the Go form is pkg/remoteexec). Keep the two in sync.
#
# API (all functions take the unit name; artifacts live under DURABLE_DIR):
#   durable_launch       <unit> <script_path>   # launch a runner script durably
#   durable_launch_cmd   <unit> <command...>    # launch an inline command durably
#   durable_is_active    <unit>                 # exit 0 iff the unit is active
#   durable_main_pid     <unit>                 # echo the unit MainPID (0 if none)
#   durable_wait_sentinel <unit> [timeout_s]    # block until done; echo exit code
#   durable_fetch_log    <unit>                 # cat the captured combined output
#   durable_stop         <unit>                 # stop + reap + remove artifacts
#
# NEVER pipe the long command through `tail -N` (buffers until exit) — the runner
# redirects to a log file read independently via durable_fetch_log.

# Resolve artifact dir + the user runtime dir up front (no hardcoded uid/home).
: "${DURABLE_DIR:=${XDG_CACHE_HOME:-$HOME/.cache}/remoteexec}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

_durable_paths() {
  # $1 = unit (without .service); sets _D_RUNNER/_D_LOG/_D_SENTINEL/_D_UNIT
  local u="${1%.service}"
  _D_UNIT="${u}.service"
  _D_RUNNER="${DURABLE_DIR}/${u}.runner.sh"
  _D_LOG="${DURABLE_DIR}/${u}.log"
  _D_SENTINEL="${DURABLE_DIR}/${u}.COMPLETE"
}

# Write the wrapper runner script for an arbitrary body, capturing combined
# output to the log and ALWAYS recording the exit code in the sentinel.
_durable_write_runner() {
  local body="$1"
  mkdir -p "${DURABLE_DIR}"
  cat >"${_D_RUNNER}" <<EOF
#!/usr/bin/env bash
set -uo pipefail
__log=${_D_LOG@Q}
__sentinel=${_D_SENTINEL@Q}
{
${body}
} >"\$__log" 2>&1
__rc=\$?
printf '%s\n' "\$__rc" > "\$__sentinel"
exit \$__rc
EOF
  chmod 0755 "${_D_RUNNER}"
}

# durable_launch <unit> <script_path>
durable_launch() {
  local unit="$1" script_path="$2"
  _durable_paths "$unit"
  _durable_write_runner "bash ${script_path@Q}"
  _durable_start "${unit%.service}"
}

# durable_launch_cmd <unit> <command...>
durable_launch_cmd() {
  local unit="$1"; shift
  _durable_paths "$unit"
  _durable_write_runner "$*"
  _durable_start "${unit%.service}"
}

# _durable_start <unit-bare> — linger + systemd-run --user (the durability core).
_durable_start() {
  local bare="$1"
  loginctl enable-linger >/dev/null 2>&1 || true
  systemctl --user reset-failed "${bare}.service" >/dev/null 2>&1 || true
  systemd-run --user --unit="${bare}" --collect bash "${_D_RUNNER}"
}

durable_is_active() {
  _durable_paths "$1"
  [ "$(systemctl --user is-active "${_D_UNIT}" 2>/dev/null)" = "active" ]
}

durable_main_pid() {
  _durable_paths "$1"
  local pid
  pid="$(systemctl --user show -p MainPID --value "${_D_UNIT}" 2>/dev/null)"
  printf '%s\n' "${pid:-0}"
}

# durable_wait_sentinel <unit> [timeout_s]  — echoes the recorded exit code.
durable_wait_sentinel() {
  _durable_paths "$1"
  local timeout="${2:-0}" waited=0
  while :; do
    if [ -f "${_D_SENTINEL}" ]; then
      cat "${_D_SENTINEL}"
      return 0
    fi
    if [ "$timeout" -gt 0 ] && [ "$waited" -ge "$timeout" ]; then
      echo "durable_wait_sentinel: ${_D_SENTINEL} did not appear within ${timeout}s" >&2
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

durable_fetch_log() {
  _durable_paths "$1"
  cat "${_D_LOG}" 2>/dev/null
}

durable_stop() {
  _durable_paths "$1"
  systemctl --user stop "${_D_UNIT}" >/dev/null 2>&1 || true
  systemctl --user reset-failed "${_D_UNIT}" >/dev/null 2>&1 || true
  rm -f "${_D_RUNNER}" "${_D_LOG}" "${_D_SENTINEL}" >/dev/null 2>&1 || true
}
