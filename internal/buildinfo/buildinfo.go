// Package buildinfo reports what the running binary was built from, using the
// version control metadata the Go toolchain stamps into main packages built
// inside a repository (go build -buildvcs). Nothing is injected at link time.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Info describes the build.
type Info struct {
	// Version is the main module version: the git tag when HEAD is tagged,
	// otherwise a pseudo-version, or "(devel)" without VCS metadata.
	Version string
	// Revision is the full VCS commit hash.
	Revision string
	// Time is the commit time in RFC 3339, as stamped by the toolchain.
	Time string
	// Modified is true when the working tree had uncommitted changes.
	Modified bool
	// GoVersion is the toolchain that built the binary.
	GoVersion string
}

// Get reads the build information of the running binary.
func Get() Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return Info{Version: "unknown", Revision: "unknown", GoVersion: runtime.Version()}
	}
	return FromBuildInfo(bi)
}

// FromBuildInfo extracts Info from a debug.BuildInfo.
func FromBuildInfo(bi *debug.BuildInfo) Info {
	info := Info{Version: bi.Main.Version, GoVersion: bi.GoVersion}
	if info.Version == "" {
		info.Version = "(devel)"
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Revision = s.Value
		case "vcs.time":
			info.Time = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	if info.Revision == "" {
		info.Revision = "unknown"
	}
	return info
}

// ShortRevision is the first 12 characters of the revision.
func (i Info) ShortRevision() string {
	if len(i.Revision) > 12 {
		return i.Revision[:12]
	}
	return i.Revision
}

// String renders one line: version, revision, dirty marker, commit time, Go version.
func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s", i.Version, i.ShortRevision())
	if i.Modified {
		b.WriteString("-dirty")
	}
	if i.Time != "" {
		fmt.Fprintf(&b, ", %s", i.Time)
	}
	fmt.Fprintf(&b, ") %s", i.GoVersion)
	return b.String()
}
