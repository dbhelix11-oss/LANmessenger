// Package version is the single source of truth for lanmessenger's release
// version, shared by every client and the relay so the two can negotiate
// compatibility and self-update can compare "is there something newer."
package version

import (
	"strconv"
	"strings"
)

// Version is this binary's release version (major.minor.patch). Bump it as
// part of cutting a release; every cmd/* binary and internal/clientcore and
// internal/servercore import this rather than hardcoding their own.
const Version = "0.1.0"

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
