// Package buildinfo reports the product identity stamped into Go executables.
// It is independent of the image tag and the Fabric schema version.
package buildinfo

import (
	"fmt"
	"strconv"
)

// These strings are set by the build-info generator using go build -ldflags -X.
var productVersion = "dev"
var buildNumber string
var sourceRevision = "unknown"
var dirty string

// Info is the product build identity shared by WeCoLab executables.
// A zero build number and an unknown revision mean the binary was not stamped.
type Info struct {
	ProductVersion string `json:"productVersion"`
	BuildNumber    int    `json:"buildNumber"`
	SourceRevision string `json:"sourceRevision"`
	Dirty          bool   `json:"dirty"`
}

var current = func() Info {
	info := Info{ProductVersion: productVersion, SourceRevision: sourceRevision, Dirty: dirty == "true"}
	if n, err := strconv.Atoi(buildNumber); err == nil && n > 0 {
		info.BuildNumber = n
	}
	return info
}()

// Current returns the identity embedded in this executable. An unstamped Go
// build reports development/unknown rather than pretending to be a release.
func Current() Info { return current }

// String displays the product identity, omitting a build number and revision
// when the executable was not stamped.
func (i Info) String() string {
	version := i.ProductVersion
	if version == "" {
		version = "dev"
	}
	out := "WeCoLab " + version
	if i.BuildNumber > 0 {
		out += fmt.Sprintf(" (build %d)", i.BuildNumber)
	}
	if i.SourceRevision != "" && i.SourceRevision != "unknown" {
		out += " revision " + i.SourceRevision
	} else {
		out += " revision unknown"
	}
	if i.Dirty {
		out += " dirty"
	}
	return out
}
