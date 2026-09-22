package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lanmessenger/internal/crypto"
)

// testKeypair returns a throwaway Ed25519 keypair for signing test manifests
// — never the real embedded UpdatePubKey, which stays empty in this repo.
func testKeypair(t *testing.T) (pub string, priv ed25519.PrivateKey) {
	t.Helper()
	p, s, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	return crypto.EncodeSignPub(p), s
}

// newTestFetcher points a Fetcher at an httptest server, bypassing NewFetcher
// (which hardcodes https:// and the real, currently-empty, UpdatePubKey).
func newTestFetcher(serverURL, pubKeyB64, stateDir string) *Fetcher {
	return &Fetcher{
		httpClient: http.DefaultClient,
		baseURL:    serverURL,
		pubKeyB64:  pubKeyB64,
		stateDir:   stateDir,
	}
}

func mustMarshal(t *testing.T, m Manifest) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return raw
}

// serveManifest starts a test relay serving one signed manifest at the usual
// /updates/ routes, signed with priv.
func serveManifest(t *testing.T, m Manifest, priv ed25519.PrivateKey) *httptest.Server {
	t.Helper()
	body := mustMarshal(t, m)
	sig := crypto.Sign(priv, body)

	mux := http.NewServeMux()
	mux.HandleFunc("/updates/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	mux.HandleFunc("/updates/manifest.json.sig", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sig))
	})
	return httptest.NewServer(mux)
}

func TestFetchManifestValidSignature(t *testing.T) {
	pub, priv := testKeypair(t)
	m := Manifest{
		Seq:         1,
		GeneratedAt: time.Now(),
		Artifacts: []Artifact{
			{Target: "lanmsg-cli", OS: "linux", Arch: "amd64", Version: "0.2.0", URL: "/updates/artifacts/x", SHA256: "deadbeef"},
		},
	}
	srv := serveManifest(t, m, priv)
	defer srv.Close()

	f := newTestFetcher(srv.URL, pub, t.TempDir())
	got, err := f.FetchManifest(context.Background())
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if got.Seq != 1 || len(got.Artifacts) != 1 {
		t.Fatalf("unexpected manifest: %+v", got)
	}
}

func TestFetchManifestTamperedRejected(t *testing.T) {
	pub, priv := testKeypair(t)
	m := Manifest{Seq: 1, Artifacts: []Artifact{{Target: "lanmsg-cli", OS: "linux", Arch: "amd64", Version: "0.2.0"}}}
	body := mustMarshal(t, m)
	sig := crypto.Sign(priv, body)

	// Serve a manifest that differs from what was signed.
	mux := http.NewServeMux()
	mux.HandleFunc("/updates/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		tampered := mustMarshal(t, Manifest{Seq: 999, Artifacts: m.Artifacts})
		w.Write(tampered)
	})
	mux.HandleFunc("/updates/manifest.json.sig", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sig))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newTestFetcher(srv.URL, pub, t.TempDir())
	if _, err := f.FetchManifest(context.Background()); err == nil {
		t.Fatal("expected signature verification to fail on tampered manifest")
	}
}

func TestFetchManifestWrongKeyRejected(t *testing.T) {
	_, priv := testKeypair(t)
	otherPub, _ := testKeypair(t) // a different keypair's public half

	m := Manifest{Seq: 1, Artifacts: nil}
	srv := serveManifest(t, m, priv)
	defer srv.Close()

	f := newTestFetcher(srv.URL, otherPub, t.TempDir())
	if _, err := f.FetchManifest(context.Background()); err == nil {
		t.Fatal("expected verification to fail against the wrong public key")
	}
}

func TestFetchManifestNoPubKeyConfigured(t *testing.T) {
	f := newTestFetcher("https://unused.invalid", "", t.TempDir())
	if _, err := f.FetchManifest(context.Background()); err == nil {
		t.Fatal("expected an explicit error when no release public key is configured")
	}
}

func TestFetchManifestRollbackRejected(t *testing.T) {
	pub, priv := testKeypair(t)
	stateDir := t.TempDir()

	// First, a legitimately newer manifest at seq=5.
	srv5 := serveManifest(t, Manifest{Seq: 5}, priv)
	f := newTestFetcher(srv5.URL, pub, stateDir)
	if _, err := f.FetchManifest(context.Background()); err != nil {
		t.Fatalf("first fetch (seq=5): %v", err)
	}
	srv5.Close()

	// Now simulate a stale/compromised relay replaying an older, still
	// validly-signed manifest at seq=3. The client has already seen seq=5.
	srv3 := serveManifest(t, Manifest{Seq: 3}, priv)
	defer srv3.Close()
	f2 := newTestFetcher(srv3.URL, pub, stateDir)
	if _, err := f2.FetchManifest(context.Background()); err == nil {
		t.Fatal("expected a lower-seq manifest to be rejected as a possible rollback")
	}
}

func TestFetchManifestEqualSeqRejected(t *testing.T) {
	pub, priv := testKeypair(t)
	stateDir := t.TempDir()

	srv := serveManifest(t, Manifest{Seq: 2}, priv)
	defer srv.Close()
	f := newTestFetcher(srv.URL, pub, stateDir)
	if _, err := f.FetchManifest(context.Background()); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	// Same seq again (e.g. a duplicate/replayed response) must not be
	// accepted as "new."
	if _, err := f.FetchManifest(context.Background()); err == nil {
		t.Fatal("expected a repeated seq to be rejected")
	}
}

func TestManifestArtifactFor(t *testing.T) {
	m := Manifest{Artifacts: []Artifact{
		{Target: "lanmsg-cli", OS: "linux", Arch: "arm64", Version: "1.0.0"},
		{Target: "lanmsg-remote-cli", OS: "android", Arch: "arm64", Version: "1.0.0"},
	}}
	a, ok := m.ArtifactFor("lanmsg-cli", "linux", "arm64")
	if !ok || a.Version != "1.0.0" {
		t.Fatalf("ArtifactFor did not find the expected entry: %+v, %v", a, ok)
	}
	if _, ok := m.ArtifactFor("lanmsg-cli", "windows", "amd64"); ok {
		t.Fatal("ArtifactFor matched a nonexistent (os, arch)")
	}
}

func TestCheckSelf(t *testing.T) {
	if !CheckSelf(Artifact{Version: "99.0.0"}) {
		t.Fatal("expected 99.0.0 to be newer than the running build")
	}
	if CheckSelf(Artifact{Version: "0.0.1"}) {
		t.Fatal("expected 0.0.1 to not be newer than the running build")
	}
}

func TestDownloadAndVerifySuccess(t *testing.T) {
	content := []byte("pretend binary contents")
	rawSum := sha256.Sum256(content)
	sum := hex.EncodeToString(rawSum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/updates/artifacts/thing", func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newTestFetcher(srv.URL, "", t.TempDir())
	destDir := t.TempDir()
	a := Artifact{Target: "lanmsg-cli", URL: "/updates/artifacts/thing", SHA256: sum}

	tmpPath, err := f.DownloadAndVerify(context.Background(), a, destDir)
	if err != nil {
		t.Fatalf("DownloadAndVerify: %v", err)
	}
	if filepath.Dir(tmpPath) != destDir {
		t.Fatalf("temp file not in destDir: %s", tmpPath)
	}
	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("downloaded content mismatch: %q", got)
	}
}

func TestDownloadAndVerifySHA256Mismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/updates/artifacts/thing", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("corrupted or tampered bytes"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newTestFetcher(srv.URL, "", t.TempDir())
	destDir := t.TempDir()
	a := Artifact{Target: "lanmsg-cli", URL: "/updates/artifacts/thing", SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}

	tmpPath, err := f.DownloadAndVerify(context.Background(), a, destDir)
	if err == nil {
		t.Fatal("expected a sha256 mismatch error")
	}
	if tmpPath != "" {
		t.Fatal("expected no path returned on failure")
	}
	entries, _ := os.ReadDir(destDir)
	if len(entries) != 0 {
		t.Fatalf("expected the temp file to be cleaned up, found: %v", entries)
	}
}

func TestSwapAtomicity(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "lanmsg-cli")
	if err := os.WriteFile(execPath, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("seed old binary: %v", err)
	}
	tmpPath := filepath.Join(dir, ".update-lanmsg-cli-new")
	if err := os.WriteFile(tmpPath, []byte("new binary"), 0o644); err != nil {
		t.Fatalf("seed new binary: %v", err)
	}

	if err := Swap(execPath, tmpPath); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("read execPath after swap: %v", err)
	}
	if string(got) != "new binary" {
		t.Fatalf("execPath content after swap = %q, want %q", got, "new binary")
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("expected tmpPath to be gone after swap (renamed), stat err = %v", err)
	}
	info, err := os.Stat(execPath)
	if err != nil {
		t.Fatalf("stat execPath: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("expected execPath to be executable after swap, mode = %v", info.Mode())
	}
}
