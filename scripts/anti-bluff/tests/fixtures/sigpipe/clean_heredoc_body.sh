#!/usr/bin/env bash
# CLEAN FIXTURE — must NOT be flagged.
# The `yes | head -c` below lives inside a heredoc. That is a DIFFERENT
# program's source; this file's `set` line does not govern it, and there
# the SIGPIPE on `yes` is the intended way to stop an infinite producer.
set -uo pipefail
cat > "$WORK/bin/journalctl" <<STUB
#!/usr/bin/env bash
yes "an ordinary uninteresting log line for padding" \
  | head -c 67108864
exit 0
STUB
chmod +x "$WORK/bin/journalctl"
