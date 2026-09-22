package servercore

import (
	"net/http"
	"path/filepath"
	"strings"
)

// This file serves the relay's self-update manifest and artifacts (see
// internal/update and cmd/lanmsg-signrelease) from cfg.UpdatesDir(). It is
// pure distribution: the relay never generates, signs, or validates any of
// this content — it just serves whatever a release workflow placed there,
// read-only. Clients are the only ones who verify anything (the manifest
// signature and each artifact's SHA-256), so there is deliberately no
// authentication on these routes beyond TLS: the files are public release
// info by design, and the signature — not secrecy — is the actual trust
// boundary. See docs/DESIGN.md §12.2.
//
// A relay with nothing ever published here (UpdatesDir doesn't exist, or is
// empty) is a normal, valid state: these handlers simply 404, and a client's
// Fetcher treats that as "no update available right now," not an error
// worth surfacing.

func (s *Server) handleUpdatesManifest(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(s.cfg.UpdatesDir(), "manifest.json"))
}

func (s *Server) handleUpdatesSig(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(s.cfg.UpdatesDir(), "manifest.json.sig"))
}

// handleUpdatesArtifact serves individual artifact files under
// UpdatesDir()/artifacts/. http.Dir (which backs http.FileServer) already
// safely confines requests to that directory — Go's stdlib cleans the
// request path and rejects anything that would resolve outside it — but the
// explicit check below is cheap, makes that guarantee visible at the call
// site rather than implicit in a library's behavior, and fails closed if
// that ever changes.
func (s *Server) handleUpdatesArtifact(w http.ResponseWriter, r *http.Request) {
	artifactsDir := filepath.Join(s.cfg.UpdatesDir(), "artifacts")

	rel := strings.TrimPrefix(r.URL.Path, "/updates/artifacts/")
	resolved := filepath.Join(artifactsDir, filepath.Clean("/"+rel))
	if resolved != artifactsDir && !strings.HasPrefix(resolved, artifactsDir+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}

	http.StripPrefix("/updates/artifacts/", http.FileServer(http.Dir(artifactsDir))).ServeHTTP(w, r)
}
