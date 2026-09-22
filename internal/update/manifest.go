// Package update implements the client side of lanmessenger's self-update
// convenience layer: fetching a signed manifest from the relay, deciding
// whether a newer build exists for this binary's own (target, OS, arch), and
// — if the caller asks for it — downloading, verifying, and swapping it in.
//
// This is deliberately separate from the coarse protocol-compatibility gate
// in internal/proto's Ready frame (see internal/clientcore's handling of
// it): that gate is "can this build talk to the relay at all," rarely
// changes, and applies uniformly to every client. This package is "is there
// a newer convenience build for exactly this binary," changes on every
// release, and is per-(target, OS, arch) — a release that only touches
// lanmsg-cli never tells a GUI client anything changed for it.
//
// internal/clientcore never imports this package, and never will: the GUI
// shares clientcore, and the GUI is deliberately excluded from ever
// auto-swapping its own binary (see cmd/lanmsg-cli and
// cmd/lanmsg-remote-cli, the only callers).
package update

import "time"

// Artifact describes one released binary for one (target, OS, arch) triple.
// Target is the binary's name (e.g. "lanmsg-cli", "lanmsg-remote-cli",
// "lanmsg" for the GUI — included for display/manual-download only, since
// self-update never runs for the GUI). OS/Arch match runtime.GOOS/GOARCH.
type Artifact struct {
	Target  string `json:"target"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
	URL     string `json:"url"` // relative to the relay's own address, or absolute
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// Manifest is the signed release manifest served at /updates/manifest.json.
// Seq is a monotonically increasing counter the signing tool bumps on every
// release; Fetcher.FetchManifest rejects any manifest whose Seq is not newer
// than the highest one this client has ever seen, closing a replay/rollback
// gap a compromised or stale relay could otherwise exploit.
type Manifest struct {
	Seq         int64      `json:"seq"`
	GeneratedAt time.Time  `json:"generated_at"`
	Artifacts   []Artifact `json:"artifacts"`
}

// ArtifactFor finds the manifest entry matching target, os, and arch exactly
// (e.g. runtime.GOOS, runtime.GOARCH), or reports it isn't present.
func (m *Manifest) ArtifactFor(target, goos, goarch string) (Artifact, bool) {
	for _, a := range m.Artifacts {
		if a.Target == target && a.OS == goos && a.Arch == goarch {
			return a, true
		}
	}
	return Artifact{}, false
}
