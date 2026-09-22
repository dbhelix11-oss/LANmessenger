// Package version is the single source of truth for lanmessenger's release
// version, shared by every client and the relay so the two can negotiate
// compatibility and self-update can compare "is there something newer."
package version

import (
	"runtime/debug"
	"strconv"
	"strings"
)

// Version is this binary's release version (major.minor.patch). Bump it as
// part of cutting a release; every cmd/* binary and internal/clientcore and
// internal/servercore import this rather than hardcoding their own.
const Version = "0.1.0"

// BuildInfo returns a short, human-readable build identifier — a 7-char git
// commit hash, plus "-dirty" if the working tree had uncommitted changes at
// build time — for confirming a running binary is actually the build you
// think it is (distinct from Version, which is only bumped for releases and
// won't change between two local rebuilds of the same commit). Needs no
// ldflags or build-script changes: `go build` embeds this automatically for
// any build made from within a git checkout (since Go 1.18), and it survives
// -trimpath, -ldflags="-s -w", and fyne package unchanged — confirmed via
// `go version -m` on an actual fyne-packaged binary.
//
// Empty if built without VCS info available (e.g. `go install` from the
// module cache rather than a local checkout) — callers should treat that as
// "unknown," not an error.
func BuildInfo() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return formatBuildInfo(info.Settings)
}

// formatBuildInfo is BuildInfo's pure formatting logic, split out so it's
// testable without depending on the ambient build environment actually
// having VCS info available — notably, `go test` itself does not embed it
// the way `go build`/`go install` of a real binary does, confirmed
// empirically against this repo, so a test exercising BuildInfo() directly
// would always skip.
func formatBuildInfo(settings []debug.BuildSetting) string {
	var rev string
	var dirty bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		return rev + "-dirty"
	}
	return rev
}

// Compare returns -1, 0, or 1 as a compares less than, equal to, or greater
// than b, treating each as major.minor.patch. Missing or non-numeric
// components are treated as 0, so "1.2" compares equal to "1.2.0" and "" is
// the smallest possible version.
func Compare(a, b string) int {
	pa, pb := parts(a), parts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Newer reports whether a is strictly newer than b.
func Newer(a, b string) bool {
	return Compare(a, b) > 0
}

func parts(v string) [3]int {
	var out [3]int
	fields := strings.SplitN(v, ".", 3)
	for i := 0; i < len(fields) && i < 3; i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		out[i] = n
	}
	return out
}
