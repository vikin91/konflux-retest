package refs

import (
	"regexp"
	"strings"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ValidRepository reports whether repo has the OWNER/REPO form.
func ValidRepository(repo string) bool {
	return repositoryPattern.MatchString(repo)
}

// ValidTag rejects ref expressions and misplaced flags while allowing tag namespaces.
func ValidTag(tag string) bool {
	if strings.HasPrefix(tag, "-") || strings.HasSuffix(tag, ".") ||
		strings.ContainsAny(tag, "~^:?*[\\") || strings.Contains(tag, "..") ||
		strings.Contains(tag, "@{") || strings.ContainsFunc(tag, func(r rune) bool { return r <= ' ' || r == 127 }) {
		return false
	}
	for part := range strings.SplitSeq(tag, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
