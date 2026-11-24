package pid

import "strings"

// isZombieFromStat inspects the contents of a /proc/<pid>/stat line
// and returns true if the state field is 'Z'.
//
// The state field is the byte right after the closing paren of the
// comm field. The comm field may itself contain spaces and
// parentheses (the kernel escapes them as \040 and \011 but the raw
// bytes are still present), so the closing paren is located with the
// LAST ')' in the line rather than the first.
//
// This parser is deliberately platform-neutral: it only manipulates a
// string, so it can be unit-tested on any platform even though only
// Unix builds read /proc. See
// docs/design/stale-pid-file-blocks-gateway-startup.md.
func isZombieFromStat(content string) bool {
	rparen := strings.LastIndex(content, ")")
	if rparen < 0 || rparen+2 >= len(content) {
		return false
	}
	return content[rparen+2] == 'Z'
}
