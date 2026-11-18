package update

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/version"
)

// UpdatePubKey is the offline release-signing key's public half (base64,
// same encoding as internal/crypto's other public keys), compiled into every
// client. The matching private key never lives in this repo, on the relay,
// or in CI — see cmd/lanmsg-signrelease, which generates it on a trusted
// machine kept separate from both.
//
// Empty until a real release key exists: FetchManifest fails closed with a
// clear error rather than attempting to verify against nothing.
const UpdatePubKey = "+Whv+SI6XAnsjfHAP0RdiLNvg5BaxEoRYsBSdPk+1ZQ="

// maxManifestBytes bounds the manifest + signature fetch; both are small
// JSON/text documents, so this is generous headroom, not a real limit.
const maxManifestBytes = 8 << 20

// Fetcher talks to one relay's /updates/ routes to check for and retrieve
// self-update artifacts.
type Fetcher struct {
	httpClient *http.Client
	baseURL    string
	pubKeyB64  string
	stateDir   string
}

// NewFetcher builds a Fetcher for the relay at serverAddr (host:port),
// reusing the same TLS config (and therefore the same pinned certificate)
// the caller's clientcore connection already trusts. socksProxy, when
// non-empty, routes every request through that SOCKS5 proxy instead of
// dialing directly — pass the same value as the client's own
// cfg.SOCKSProxy (e.g. lanmsg-remote-cli over Tor); empty means dial
// directly, the right choice for a plain LAN client. stateDir is where the
// anti-rollback high-water mark (the highest manifest Seq seen) persists
// across runs — callers should pass their own config directory.
func NewFetcher(serverAddr string, tlsConfig *tls.Config, socksProxy string, stateDir string) *Fetcher {
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	if socksProxy != "" {
		transport.DialContext = socksDialContext(socksProxy)
	}
	return &Fetcher{
		httpClient: &http.Client{Transport: transport},
		baseURL:    "https://" + serverAddr,
		pubKeyB64:  UpdatePubKey,
		stateDir:   stateDir,
	}
}

// FetchManifest retrieves and verifies the relay's current manifest: the
// signature must check out against UpdatePubKey, and its Seq must be
// strictly newer than the highest this client has ever seen. Either failure
// discards the manifest entirely — "fail ⇒ stop, update ignored" — rather
// than falling back to some partial trust.
func (f *Fetcher) FetchManifest(ctx context.Context) (*Manifest, error) {
	m, err := f.fetchVerified(ctx)
	if err != nil {
		return nil, err
	}
	lastSeq, err := f.loadSeq()
	if err != nil {
		return nil, err
	}
	if m.Seq <= lastSeq {
		return nil, fmt.Errorf("update: manifest seq %d is not newer than last seen %d (possible rollback)", m.Seq, lastSeq)
	}
	if err := f.saveSeq(m.Seq); err != nil {
		return nil, fmt.Errorf("update: persist manifest seq: %w", err)
	}
	return m, nil
}

// CheckManifest fetches and verifies the manifest like FetchManifest, but for
// a caller that only wants to know what is on offer: an equal sequence number
// is fine (the same list seen again), an older one is rejected as a rollback,
// and nothing is written. A long-running program that shows a notice uses this
// and calls RestoreSeq(m.Seq) only once it has actually installed a release.
func (f *Fetcher) CheckManifest(ctx context.Context) (*Manifest, error) {
	m, err := f.fetchVerified(ctx)
	if err != nil {
		return nil, err
	}
	lastSeq, err := f.loadSeq()
	if err != nil {
		return nil, err
	}
	if m.Seq < lastSeq {
		return nil, fmt.Errorf("update: manifest seq %d is older than last seen %d (possible rollback)", m.Seq, lastSeq)
	}
	return m, nil
}

// fetchVerified downloads the manifest and its signature, checks the
// signature against the compiled-in release key and parses it.
func (f *Fetcher) fetchVerified(ctx context.Context) (*Manifest, error) {
	if f.pubKeyB64 == "" {
		return nil, fmt.Errorf("update: no release public key compiled in; self-update is disabled until one is configured (see cmd/lanmsg-signrelease)")
	}

	body, err := f.getBytes(ctx, "/updates/manifest.json")
	if err != nil {
		return nil, fmt.Errorf("update: fetch manifest: %w", err)
	}
	sigBytes, err := f.getBytes(ctx, "/updates/manifest.json.sig")
	if err != nil {
		return nil, fmt.Errorf("update: fetch manifest signature: %w", err)
	}
	sig := strings.TrimSpace(string(sigBytes))

	ok, err := crypto.VerifyFromB64Pub(f.pubKeyB64, body, sig)
	if err != nil {
		return nil, fmt.Errorf("update: bad release public key: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("update: manifest signature verification failed")
	}

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("update: parse manifest: %w", err)
	}

	return &m, nil
}

// LastSeq returns the highest manifest Seq this client has accepted.
func (f *Fetcher) LastSeq() (int64, error) { return f.loadSeq() }

// RestoreSeq sets the high-water mark back to seq. A caller uses it when a
// manifest was accepted but its artifact then failed to download or verify,
// so the next run retries the same release instead of rejecting it as "not
// newer". It never lowers protection for a release that was fully installed.
func (f *Fetcher) RestoreSeq(seq int64) error { return f.saveSeq(seq) }

// CheckSelf reports whether a is strictly newer than this running binary's
// own version.Version.
func CheckSelf(a Artifact) bool {
	return version.Newer(a.Version, version.Version)
}

// DownloadAndVerify downloads a's artifact into a new temp file in destDir
// and checks its SHA-256 against the manifest entry, discarding the file on
// any mismatch. destDir should be the directory the running executable
// itself lives in, so the caller's later Swap (a same-filesystem rename) is
// atomic. It never touches the live binary — that's Swap's job alone.
func (f *Fetcher) DownloadAndVerify(ctx context.Context, a Artifact, destDir string) (string, error) {
	url := a.URL
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = f.baseURL + a.URL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("update: download %s: %w", a.Target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update: download %s: unexpected status %s", a.Target, resp.Status)
	}

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", fmt.Errorf("update: dest dir: %w", err)
	}
	tmp, err := os.CreateTemp(destDir, ".update-"+a.Target+"-*")
	if err != nil {
		return "", fmt.Errorf("update: create temp file: %w", err)
	}
	path := tmp.Name()
	success := false
	defer func() {
		tmp.Close()
		if !success {
			os.Remove(path)
		}
	}()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return "", fmt.Errorf("update: download %s: %w", a.Target, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("update: finalize download of %s: %w", a.Target, err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, a.SHA256) {
		return "", fmt.Errorf("update: sha256 mismatch for %s: got %s, want %s", a.Target, got, a.SHA256)
	}
	success = true
	return path, nil
}

func (f *Fetcher) getBytes(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s for %s", resp.Status, path)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
}

// --- anti-rollback state: highest manifest Seq ever seen -------------------

type updateState struct {
	Seq int64 `json:"seq"`
}

func (f *Fetcher) statePath() string {
	return filepath.Join(f.stateDir, "update_state.json")
}

func (f *Fetcher) loadSeq() (int64, error) {
	raw, err := os.ReadFile(f.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("update: read state: %w", err)
	}
	var s updateState
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("update: parse state: %w", err)
	}
	return s.Seq, nil
}

func (f *Fetcher) saveSeq(seq int64) error {
	raw, err := json.Marshal(updateState{Seq: seq})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		return fmt.Errorf("update: state dir: %w", err)
	}
	tmp := f.statePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("update: write state: %w", err)
	}
	return os.Rename(tmp, f.statePath())
}
