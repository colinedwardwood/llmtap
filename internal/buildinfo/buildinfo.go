// Package buildinfo carries values stamped in at link time via -ldflags -X.
// Defaults make `go run ./...` and unit tests work without a build step;
// `go install` users get vcs information back via runtime/debug.ReadBuildInfo
// since the linker -X variables are unset for them.
package buildinfo

import (
	"runtime/debug"
	"sync"
)

var (
	// Set via -X at link time. Empty / "dev" / "unknown" trigger the
	// runtime/debug.ReadBuildInfo fallback so `go install …@v0.1.3`
	// users don't see the raw defaults.
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

var resolveOnce sync.Once

// resolveFromBuildInfo upgrades unset / default values from
// runtime/debug.ReadBuildInfo, which `go install` populates with
// the module's resolved version + VCS metadata. Called lazily so the
// stdlib's reflection cost is paid once and only when needed.
func resolveFromBuildInfo() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if isUnset(Version) && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if isUnset(Commit) && s.Value != "" {
				Commit = s.Value
				if len(Commit) > 12 {
					Commit = Commit[:12]
				}
			}
		case "vcs.time":
			if isUnset(Date) && s.Value != "" {
				Date = s.Value
			}
		}
	}
}

// Resolve returns the (Version, Commit, Date) triple after upgrading
// any unset value from runtime/debug.ReadBuildInfo. Callers in the
// CLI (`llmtap version`) and the resource-attribute builder use this
// to present accurate version info regardless of how the binary was
// built.
func Resolve() (version, commit, date string) {
	resolveOnce.Do(resolveFromBuildInfo)
	return Version, Commit, Date
}

func isUnset(s string) bool {
	return s == "" || s == "dev" || s == "unknown"
}
