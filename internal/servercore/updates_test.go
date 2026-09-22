package servercore

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// getBody issues a GET against the running test relay and returns the
// status code and body.
func getBody(t *testing.T, addr, path string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := httpClientInsecure.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

func TestUpdatesManifestNotYetPublished(t *testing.T) {
	addr := newTestServer(t, false)

	if code, _ := getBody(t, addr, "/updates/manifest.json"); code != http.StatusNotFound {
		t.Fatalf("manifest.json status = %d, want 404", code)
	}
	if code, _ := getBody(t, addr, "/updates/manifest.json.sig"); code != http.StatusNotFound {
		t.Fatalf("manifest.json.sig status = %d, want 404", code)
	}
	if code, _ := getBody(t, addr, "/updates/artifacts/nope"); code != http.StatusNotFound {
		t.Fatalf("artifacts/nope status = %d, want 404", code)
	}
}

func TestUpdatesServesPublishedManifestAndArtifact(t *testing.T) {
	var cfg *Config
	addr := newTestServerCfg(t, func(c *Config) { cfg = c })

	updatesDir := cfg.UpdatesDir()
	artifactsDir := filepath.Join(updatesDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	manifestBody := []byte(`{"seq":1,"artifacts":[]}`)
	if err := os.WriteFile(filepath.Join(updatesDir, "manifest.json"), manifestBody, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(updatesDir, "manifest.json.sig"), []byte("deadbeef=="), 0o644); err != nil {
		t.Fatalf("write sig: %v", err)
	}
	artifactBody := []byte("pretend binary contents")
	if err := os.WriteFile(filepath.Join(artifactsDir, "lanmsg-cli-linux-arm64-0.2.0"), artifactBody, 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	if code, body := getBody(t, addr, "/updates/manifest.json"); code != http.StatusOK || string(body) != string(manifestBody) {
		t.Fatalf("manifest: status=%d body=%q", code, body)
	}
	if code, body := getBody(t, addr, "/updates/manifest.json.sig"); code != http.StatusOK || string(body) != "deadbeef==" {
		t.Fatalf("sig: status=%d body=%q", code, body)
	}
	if code, body := getBody(t, addr, "/updates/artifacts/lanmsg-cli-linux-arm64-0.2.0"); code != http.StatusOK || string(body) != string(artifactBody) {
		t.Fatalf("artifact: status=%d body=%q", code, body)
	}
}

func TestUpdatesArtifactPathTraversalRejected(t *testing.T) {
	var cfg *Config
	addr := newTestServerCfg(t, func(c *Config) { cfg = c })

	artifactsDir := filepath.Join(cfg.UpdatesDir(), "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	// A file that genuinely exists just outside the artifacts dir, that a
	// traversal attempt might try to reach.
	secret := filepath.Join(cfg.UpdatesDir(), "secret")
	if err := os.WriteFile(secret, []byte("should never be served"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	for _, path := range []string{
		"/updates/artifacts/../secret",
		"/updates/artifacts/..%2Fsecret",
		"/updates/artifacts/../../etc/passwd",
	} {
		code, body := getBody(t, addr, path)
		if code == http.StatusOK && string(body) == "should never be served" {
			t.Fatalf("path traversal succeeded via %q: got the secret file", path)
		}
	}
}
