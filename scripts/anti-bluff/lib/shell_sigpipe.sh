#!/usr/bin/env bash
# SIGPIPE-VERDICT matcher — the detector half of the SIGPIPE-V gate.
#
# THE DEFECT CLASS
# ----------------
# Under `set -o pipefail`, a pipeline whose LAST stage stops reading as soon as
# it has what it needs is a trap when its exit status decides a verdict:
#
#     if echo "$body" | grep -q MARKER; then ...
#
# `grep -q` exits the instant it matches. The writer on the left is then killed
# by SIGPIPE (status 141). `pipefail` promotes 141 to the pipeline's status. So
# the pipeline reports FAILURE **because the pattern was found**.
#
# Measured on this host, not theorised. `echo "$V" | grep -q PAT`, marker
# present on every iteration, 200 iterations per row, MULTI-LINE body:
#
#     producer bytes   marker at 1%   marker at 50%
#              4096        0 / 200        0 / 200
#             16384      200 / 200       72 / 200
#             65536      200 / 200      200 / 200
#            262144      200 / 200      200 / 200
#
# The threshold is near PIPE_BUF (4096) — NOT the 64 KiB pipe capacity. A write
# larger than PIPE_BUF is split, and a reader that closes between fragments
# delivers SIGPIPE.
#
# TWO CONDITIONS, both measured, and the second is easy to get wrong:
#   1. more than ~PIPE_BUF of output still to write after the match; and
#   2. a LINE TERMINATOR after the match. GNU grep is line-oriented: given a
#      body with no newline at all it keeps reading to EOF before deciding, so
#      the writer is never cut off. A 256 KiB SINGLE-LINE body scored 0/200
#      non-zero; the same 256 KiB with one newline after the marker scored
#      300/300 at status 141. Real producers — curl bodies, `--help` text,
#      test-runner output — are line-oriented, so they meet condition 2.
#
# Being timing- and size-dependent, this is normally written off as flake.
#
# The same trap belongs to any consumer that closes the read end early:
# `head`, `sed -n '1p'`, `sed q`, `awk '...exit'`, `read`, `grep -m N`.
#
# THE FIX is bash's own pattern matching in the verdict position:
#     [[ $body == *MARKER* ]]        [[ $body =~ $re ]]        case $body in ...
#
# WHAT THIS MATCHER DELIBERATELY DOES NOT FLAG
# --------------------------------------------
#   * files that never set pipefail — without it the trap cannot fire
#   * pipelines whose status is DISCARDED (`x=$(... | head -1)`, bare
#     statements). Churn in a gate script is its own risk; a harmless
#     pipeline is left alone.
#   * anything inside a heredoc — that text is another program's source, and
#     that program's own `set` line governs it, not this file's.
#   * `a || head -1` and friends: `||` / `&&` are masked before matching so
#     their second `|` is never read as a pipe.
#
# KNOWN LIMITATION (§11.4.6): the verdict test is single-line and syntactic.
# A pipeline that is the last command of a function whose return status is a
# verdict is NOT detected — that needs whole-file dataflow, which this does
# not do. Absence of a hit is therefore not proof of absence.
#
# Emits: <displaypath>:<line>:SIGPIPE-V-001:<context> [<why>]
# Returns 0 always; callers collect stdout.

# scan_shell_sigpipe <displaypath> <realpath>
scan_shell_sigpipe() {
  local name="$1" file="$2"
  [[ -f "$file" ]] || return 0

  # Gate 1: the file must actually enable pipefail. Covers `set -o pipefail`
  # and the bundled `set -euo pipefail` / `set -uo pipefail` forms.
  grep -qE '^[[:space:]]*set[[:space:]]+(-[a-zA-Z]*o[a-zA-Z]*[[:space:]]+pipefail|-o[[:space:]]+pipefail)' \
       "$file" || return 0

  awk -v name="$name" '
    # Consumers that may close the read end before EOF. Kept as an awk literal
    # rather than passed with -v, because -v applies backslash processing and
    # would silently mangle the expression into one that matches everything.
    function consumer_re() {
      return "[|][[:space:]]*(!?[[:space:]]*)?((command[[:space:]]+)?(grep|ggrep|egrep|fgrep)[[:space:]]+(-[A-Za-z]*q[A-Za-z]*)([[:space:]]|$)|(grep|ggrep)[^|]*[[:space:]]-m[[:space:]]*[0-9]|head([[:space:]]|$)|sed[[:space:]]+-n[[:space:]]*.?[0-9]+p|sed[[:space:]]+[0-9]*q([[:space:]]|$)|awk[^|]*[[:space:]]exit[[:space:];}]|read([[:space:]]|$))"
    }

    # --- heredoc suppression -------------------------------------------------
    # A heredoc body is a different program source. Its own `set` line governs
    # it, so this file pipefail says nothing about it.
    in_heredoc {
      probe = $0
      sub(/^[[:space:]]+/, "", probe)
      if ($0 == hd_delim || (hd_dash && probe == hd_delim)) in_heredoc = 0
      next
    }
    {
      line = $0

      if (match(line, /<<-?[[:space:]]*["'"'"']?[A-Za-z_][A-Za-z0-9_]*["'"'"']?/)) {
        d = substr(line, RSTART, RLENGTH)
        hd_dash = (d ~ /<<-/)
        sub(/^<<-?[[:space:]]*/, "", d)
        gsub(/["'"'"']/, "", d)
        hd_delim = d
        in_heredoc = 1
        next
      }

      trimmed = line
      sub(/^[[:space:]]+/, "", trimmed)
      if (trimmed ~ /^#/) next
      if (trimmed == "")  next

      # Mask || and && with same-length placeholders so their second character
      # is never mistaken for a pipe, while byte offsets stay valid for the
      # original line.
      probe = line
      gsub(/\|\|/, "\001\001", probe)
      gsub(/&&/,   "\002\002", probe)

      if (!match(probe, consumer_re())) next

      head = substr(line, 1, RSTART - 1)
      tail = substr(line, RSTART + RLENGTH)

      head_trim = head
      sub(/^[[:space:]]+/, "", head_trim)

      verdict = 0; why = ""

      # (a) the pipeline IS the condition of if / elif / while / until.
      #     Nested inside $( ) or [[ ]] its status is discarded, so require the
      #     head to have opened neither.
      if (head_trim ~ /^(if|elif|while|until)[[:space:]]/) {
        h  = head; opens     = gsub(/\$\(/, "", h)
        h2 = head; closes    = gsub(/\)/,   "", h2)
        # Count [[ against ]]: a CLOSED `[[ ... ]] &&` before the pipe is a
        # separate test joined to it, and the pipeline is still the deciding
        # command. Only an UNCLOSED `[[` means we are inside a test, where the
        # pipeline is a substitution whose status is discarded.
        h3 = head; br_open  = gsub(/\[\[/, "", h3)
        h4 = head; br_close = gsub(/\]\]/, "", h4)
        if (opens <= closes && br_open <= br_close) { verdict = 1; why = "if/while condition" }
      }

      # (b) the status is consumed by && or || further along the same line.
      #     An assignment head means command substitution, whose status is
      #     discarded, so exclude it.
      if (!verdict) {
        tail_masked = tail
        gsub(/\|\|/, "\001\001", tail_masked)
        gsub(/&&/,   "\002\002", tail_masked)
        if (tail_masked ~ /\001\001|\002\002/) {
          if (head_trim !~ /^(local[[:space:]]+|export[[:space:]]+|declare[[:space:]]+|readonly[[:space:]]+)?[A-Za-z_][A-Za-z0-9_]*=/) {
            verdict = 1; why = "&&/|| consumes pipeline status"
          }
        }
      }

      if (!verdict) next

      ctx = trimmed
      gsub(/:/, ";", ctx)                 # keep the 4-field record parseable
      printf "%s:%d:SIGPIPE-V-001:%s [%s]\n", name, NR, ctx, why
    }
  ' "$file"
  return 0
}
