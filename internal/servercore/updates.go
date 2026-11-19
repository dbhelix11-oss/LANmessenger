package servercore

import (
	"net/http"
	"os"
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

// updatesAllowed applies a per-source request limit to the update routes so a
// loop of 30 MB downloads cannot starve the small router. Tunnelled clients all
// share the loopback source, so the limit is generous (see updateLimiter).
func (s *Server) updatesAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.updateLimiter.Allow(hostOnly(r.RemoteAddr)) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "too many requests", http.StatusTooManyRequests)
	return false
}

// noListFS serves files but refuses directories, so /updates/artifacts/ no
// longer lists every build ever staged; only names the signed manifest
// mentions are worth asking for.
type noListFS struct{ fs http.FileSystem }

func (n noListFS) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

func (s *Server) handleUpdatesManifest(w http.ResponseWriter, r *http.Request) {
	if !s.updatesAllowed(w, r) {
		return
	}
	http.ServeFile(w, r, filepath.Join(s.cfg.UpdatesDir(), "manifest.json"))
}

func (s *Server) handleUpdatesSig(w http.ResponseWriter, r *http.Request) {
	if !s.updatesAllowed(w, r) {
		return
	}
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
	if !s.updatesAllowed(w, r) {
		return
	}
	artifactsDir := filepath.Join(s.cfg.UpdatesDir(), "artifacts")

	rel := strings.TrimPrefix(r.URL.Path, "/updates/artifacts/")
	resolved := filepath.Join(artifactsDir, filepath.Clean("/"+rel))
	if resolved != artifactsDir && !strings.HasPrefix(resolved, artifactsDir+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}

	http.StripPrefix("/updates/artifacts/", http.FileServer(noListFS{http.Dir(artifactsDir)})).ServeHTTP(w, r)
}
