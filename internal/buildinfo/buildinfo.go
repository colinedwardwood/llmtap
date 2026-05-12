// Package buildinfo carries values stamped in at link time via -ldflags -X.
// Defaults make `go run ./...` and unit tests work without a build step.
package buildinfo

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)
