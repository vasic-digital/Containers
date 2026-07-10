package volume

import "strings"

// shellQuote wraps s in single quotes for safe interpolation into a remote
// shell command string, escaping any embedded single quote as '\”. Volume
// paths come from caller-supplied VolumeMount specs, so a path containing a
// space would otherwise be word-split (mounting/creating the wrong directory)
// and a path containing shell metacharacters (;, $(...), backticks, &&) would
// be executed verbatim on the remote host — arbitrary command injection. With
// this quoting the path is passed to the remote shell as a single literal
// argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
