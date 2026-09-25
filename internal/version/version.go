// Package version holds build information injected at link time, e.g.
//
//	go build -ldflags "-X github.com/sl0wz3r/bunkarr/internal/version.Version=1.2.3 \
//	  -X github.com/sl0wz3r/bunkarr/internal/version.Commit=abc1234 \
//	  -X github.com/sl0wz3r/bunkarr/internal/version.BuildDate=2026-01-02T03:04:05Z"
package version

var (
	// Version is the semantic version of this build.
	Version = "0.1.0-dev"
	// Commit is the short VCS revision of this build.
	Commit = "unknown"
	// BuildDate is the RFC3339 UTC build timestamp ("" when unknown).
	BuildDate = ""
)

// UserAgent returns the User-Agent every outbound HTTP client sends: "Bunkarr/<Version>".
func UserAgent() string {
	return "Bunkarr/" + Version
}
