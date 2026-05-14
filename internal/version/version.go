// Package version provides version information for the IPAM API server.
// Version information is injected at build time using ldflags.
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the semantic version of the IPAM API server.
	Version = "dev"

	// GitCommit is the git commit hash of the build.
	GitCommit = "unknown"

	// GitTreeState indicates whether the git tree was clean or dirty during build.
	GitTreeState = "unknown"

	// BuildDate is the date when the binary was built (RFC3339 format).
	BuildDate = "unknown"
)

// Info contains version information.
type Info struct {
	Version      string `json:"version"`
	GitCommit    string `json:"gitCommit"`
	GitTreeState string `json:"gitTreeState"`
	BuildDate    string `json:"buildDate"`
	GoVersion    string `json:"goVersion"`
	Compiler     string `json:"compiler"`
	Platform     string `json:"platform"`
}

// Get returns the overall version information.
func Get() Info {
	return Info{
		Version:      Version,
		GitCommit:    GitCommit,
		GitTreeState: GitTreeState,
		BuildDate:    BuildDate,
		GoVersion:    runtime.Version(),
		Compiler:     runtime.Compiler,
		Platform:     fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}

// String returns a human-readable version string.
func (i Info) String() string {
	return fmt.Sprintf("IPAM API Server %s (commit: %s, built: %s, go: %s, platform: %s)",
		i.Version, i.GitCommit, i.BuildDate, i.GoVersion, i.Platform)
}
