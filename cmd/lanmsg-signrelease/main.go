// Command lanmsg-signrelease is the admin-only release signing tool for
// lanmessenger's self-update mechanism. It is deliberately NOT shipped to
// the Pi, not built by CI, and not distributed to any client — it exists
// solely to run on a separate, trusted machine that holds the offline
// Ed25519 release-signing private key, which must never touch the relay or
// any automated build system (see docs/DESIGN.md §12.2: "the relay is
// distribution, not authority").
//
// Usage:
//
//	lanmsg-signrelease init -key path
//	    Generate a new Ed25519 keypair and save the private half to path
//	    (0600). Prints the public half to paste into
//	    internal/update.UpdatePubKey.
//
//	lanmsg-signrelease sign -key path -spec release.json [-prev manifest.json] -out-dir dir
//	    Build and sign a manifest from the artifacts named in a release
//	    spec, merging with a previous manifest (if given) so artifacts not
//	    mentioned in this release keep their existing entry unchanged.
//	    SHA-256 is always recomputed from the actual bytes on disk, never
//	    trusted from the spec file. Writes manifest.json, manifest.json.sig,
//	    and copies each artifact into out-dir/artifacts/ under out-dir.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/update"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "sign":
		err = cmdSign(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lanmsg-signrelease — admin-only release signing tool. Run this ONLY on a
machine you trust, kept separate from the relay and from CI. See the package
doc comment (go doc ./cmd/lanmsg-signrelease) for the full explanation.

  lanmsg-signrelease init -key path
      Generate a new release signing keypair. Prints the public half to
      paste into internal/update.UpdatePubKey; the private half is written
      to path (0600) and must never leave this machine.

  lanmsg-signrelease sign -key path -spec release.json [-prev manifest.json] -out-dir dir
      Build and sign a manifest from a release spec, merging with a
      previous manifest if given.
`)
}

// --- init --------------------------------------------------------------

type releaseKeyFile struct {
	Pub  string `json:"pub"`  // base64 ed25519 public key
	Priv string `json:"priv"` // base64 ed25519 private key — KEEP OFFLINE, never commit
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	keyPath := fs.String("key", "", "path to write the private key file (required)")
	_ = fs.Parse(args)
	if *keyPath == "" {
		return errors.New("-key is required")
	}
	if _, err := os.Stat(*keyPath); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite a release key", *keyPath)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate keypair: %w", err)
	}
	f := releaseKeyFile{
		Pub:  crypto.EncodeSignPub(pub),
		Priv: base64.StdEncoding.EncodeToString(priv),
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*keyPath), 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	tmp := *keyPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write key file: %w", err)
	}
	if err := os.Rename(tmp, *keyPath); err != nil {
		return fmt.Errorf("install key file: %w", err)
	}

	fmt.Printf(`
Release signing key generated: %s

Keep that file OFFLINE — on this machine only, ideally also backed up to a
hardware key or an offline USB drive. It must never be copied to the relay,
committed to the repo, or uploaded to CI.

Paste this into internal/update.UpdatePubKey (and rebuild every client):

  const UpdatePubKey = %q
`, *keyPath, f.Pub)
	return nil
}

func loadReleaseKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	var f releaseKeyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse key file: %w", err)
	}
	priv, err := base64.StdEncoding.DecodeString(f.Priv)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("key file has a malformed private key")
	}
	return ed25519.PrivateKey(priv), nil
}

// --- sign ----------------------------------------------------------------

// releaseSpec is the small input file an admin writes (or a release script
// generates) naming which freshly built artifacts go into this release.
// Artifacts not mentioned here, but present in -prev, carry forward
// unchanged — that's what lets a CLI-only release leave the GUI's manifest
// entries untouched.
type releaseSpec struct {
	Artifacts []specArtifact `json:"artifacts"`
}

type specArtifact struct {
	Target  string `json:"target"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
	File    string `json:"file"` // local path to the built binary
}

func cmdSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "", "path to the private key file from `init` (required)")
	specPath := fs.String("spec", "", "path to the release spec JSON (required)")
	prevPath := fs.String("prev", "", "path to a previous manifest.json to merge into (optional)")
	outDir := fs.String("out-dir", "", "directory to write manifest.json, manifest.json.sig, and artifacts/ into (required)")
	_ = fs.Parse(args)
	if *keyPath == "" || *specPath == "" || *outDir == "" {
		return errors.New("-key, -spec, and -out-dir are required")
	}

	priv, err := loadReleaseKey(*keyPath)
	if err != nil {
		return err
	}

	specRaw, err := os.ReadFile(*specPath)
	if err != nil {
		return fmt.Errorf("read spec: %w", err)
	}
	var spec releaseSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return fmt.Errorf("parse spec: %w", err)
	}
	if len(spec.Artifacts) == 0 {
		return errors.New("spec has no artifacts")
	}

	var prev update.Manifest
	if *prevPath != "" {
		prevRaw, err := os.ReadFile(*prevPath)
		if err != nil {
			return fmt.Errorf("read prev manifest: %w", err)
		}
		if err := json.Unmarshal(prevRaw, &prev); err != nil {
			return fmt.Errorf("parse prev manifest: %w", err)
		}
	}

	artifactsDir := filepath.Join(*outDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return fmt.Errorf("create artifacts dir: %w", err)
	}

	merged := append([]update.Artifact(nil), prev.Artifacts...)
	for _, sa := range spec.Artifacts {
		art, err := hashAndCopyArtifact(sa, artifactsDir)
		if err != nil {
			return fmt.Errorf("artifact %s/%s/%s: %w", sa.Target, sa.OS, sa.Arch, err)
		}
		merged = replaceOrAppend(merged, art)
		fmt.Printf("  %-20s %-10s %-8s %-10s sha256:%s\n", art.Target, art.OS, art.Arch, art.Version, art.SHA256[:16]+"…")
	}

	m := update.Manifest{
		Seq:         prev.Seq + 1,
		GeneratedAt: time.Now().UTC(),
		Artifacts:   merged,
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	sig := crypto.Sign(priv, body)

	manifestPath := filepath.Join(*outDir, "manifest.json")
	sigPath := filepath.Join(*outDir, "manifest.json.sig")
	if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.WriteFile(sigPath, []byte(sig), 0o644); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}

	fmt.Printf(`
Signed release manifest written:

  %s
  %s
  %s/  (%d artifact file(s))

seq: %d (previous: %d)

Copy the contents of %s to the relay's updates/ directory.
`, manifestPath, sigPath, artifactsDir, len(spec.Artifacts), m.Seq, prev.Seq, *outDir)
	return nil
}

// hashAndCopyArtifact copies sa.File into artifactsDir under a stable,
// URL-safe name and computes its SHA-256 from the copied bytes — never from
// anything the spec claims, since the spec is just admin input, not a trust
// boundary by itself.
func hashAndCopyArtifact(sa specArtifact, artifactsDir string) (update.Artifact, error) {
	src, err := os.Open(sa.File)
	if err != nil {
		return update.Artifact{}, fmt.Errorf("open %s: %w", sa.File, err)
	}
	defer src.Close()

	name := fmt.Sprintf("%s-%s-%s-%s%s", sa.Target, sa.OS, sa.Arch, sa.Version, filepath.Ext(sa.File))
	dstPath := filepath.Join(artifactsDir, name)
	dst, err := os.Create(dstPath)
	if err != nil {
		return update.Artifact{}, fmt.Errorf("create %s: %w", dstPath, err)
	}
	defer dst.Close()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(dst, h), src)
	if err != nil {
		return update.Artifact{}, fmt.Errorf("copy %s: %w", sa.File, err)
	}
	if err := dst.Close(); err != nil {
		return update.Artifact{}, fmt.Errorf("finalize %s: %w", dstPath, err)
	}
	if err := os.Chmod(dstPath, 0o755); err != nil {
		return update.Artifact{}, fmt.Errorf("chmod %s: %w", dstPath, err)
	}

	return update.Artifact{
		Target:  sa.Target,
		OS:      sa.OS,
		Arch:    sa.Arch,
		Version: sa.Version,
		URL:     "/updates/artifacts/" + name,
		SHA256:  hex.EncodeToString(h.Sum(nil)),
		Size:    size,
	}, nil
}

// replaceOrAppend replaces the manifest entry matching art's (target, os,
// arch), or appends it if there's no existing entry for that triple.
func replaceOrAppend(artifacts []update.Artifact, art update.Artifact) []update.Artifact {
	for i, a := range artifacts {
		if a.Target == art.Target && a.OS == art.OS && a.Arch == art.Arch {
			artifacts[i] = art
			return artifacts
		}
	}
	return append(artifacts, art)
}
