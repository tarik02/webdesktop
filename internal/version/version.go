package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

var Version = "dev"

// Info contains build and runtime version details.
type Info struct {
	Version   string
	Commit    string
	GoVersion string
	Platform  string
}

// Get returns version details from linker values and Go build metadata.
func Get() Info {
	info := Info{
		Version:   Version,
		GoVersion: runtime.Version(),
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}

	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range buildInfo.Settings {
			switch setting.Key {
			case "vcs.revision":
				info.Commit = setting.Value
			}
		}
	}

	return info
}

// Short returns the release version for Cobra's version flag.
func Short() string {
	return Get().Version
}

// Details returns human-readable build information.
func Details() string {
	info := Get()
	lines := []string{
		"version: " + info.Version,
		"go: " + info.GoVersion,
		"platform: " + info.Platform,
	}
	if info.Commit != "" {
		lines = append(lines, "commit: "+info.Commit)
	}
	return strings.Join(lines, "\n")
}
