package provider

import (
	"path"
	"strings"
)

// pathSegments returns the cleaned, non-empty `/`-separated segments of
// urlPath. A trailing slash returns nil — operation endpoints are exact
// resources, not directories, so `/v1/chat/completions/` is rejected.
// An empty urlPath returns a nil slice.
func pathSegments(urlPath string) []string {
	if urlPath == "" || urlPath == "/" {
		return nil
	}
	// Trailing slash means "this is a directory/collection", not the
	// operation itself. Reject up front rather than let path.Clean
	// silently rewrite it away.
	if strings.HasSuffix(urlPath, "/") {
		return nil
	}
	cleaned := path.Clean(urlPath)
	if cleaned == "/" || cleaned == "." {
		return nil
	}
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" {
		return nil
	}
	return strings.Split(cleaned, "/")
}

// isAPIParent reports whether the segments immediately preceding an
// operation segment look like a real API namespace boundary. The
// segment immediately before the operation must be either a version
// segment (`v\d+`, case-insensitive) or, for the Azure OpenAI deployment
// shape, follow a `deployments/{name}` pair. Any other parent (e.g.
// `files`, `.well-known`) is treated as a sub-resource collision and
// the operation is rejected.
func isAPIParent(parents []string) bool {
	if len(parents) == 0 {
		return false
	}
	last := parents[len(parents)-1]
	if isVersionSegment(last) {
		return true
	}
	// Azure shape: .../deployments/{anything}/<op>
	if len(parents) >= 2 && parents[len(parents)-2] == "deployments" {
		return true
	}
	return false
}

// isVersionSegment reports whether s looks like `vN` for some
// positive-integer N (case-insensitive). Matches `v1`, `V2`, `v10`;
// rejects `version`, `v`, `v1beta`, `1`.
func isVersionSegment(s string) bool {
	if len(s) < 2 {
		return false
	}
	if s[0] != 'v' && s[0] != 'V' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
