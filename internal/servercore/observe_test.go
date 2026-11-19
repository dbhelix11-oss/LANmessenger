package servercore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lanmessenger/internal/proto"
)

func TestAdminReplayCache(t *testing.T) {
	var a adminReplayCache
	now := time.Now()
	if !a.firstUse("n1", now, time.Minute) {
		t.Fatal("first use refused")
	}
	if a.firstUse("n1", now, time.Minute) {
		t.Fatal("replay accepted")
	}
	if !a.firstUse("n1", now.Add(5*time.Minute), time.Minute) {
		t.Fatal("expired nonce should be forgotten")
	}
}

func TestRTTStats(t *testing.T) {
	var r rttStats
	if a, mn, mx := r.snapshot(); a|mn|mx != 0 {
		t.Fatal("empty stats should be zero")
	}
	r.add(10 * time.Millisecond)
	r.add(30 * time.Millisecond)
	if a, mn, mx := r.snapshot(); a != 20 || mn != 10 || mx != 30 {
		t.Fatalf("got %d/%d/%d", a, mn, mx)
	}
}

func TestFactChanges(t *testing.T) {
	s, err := openServerStore(t.TempDir() + "/x.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f := proto.ClientFacts{Version: "1", OS: "linux", Arch: "amd64"}
	if err := s.recordFacts("d", f, true, 1); err != nil {
		t.Fatal(err)
	}
	f.OS = "android"
	if err := s.recordFacts("d", f, true, 2); err != nil {
		t.Fatal(err)
	}
	var rep proto.DeviceReport
	if err := s.deviceRecords("d", &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.FactChanges) != 1 || rep.FactChanges[0].Field != "os" || rep.FactChanges[0].New != "android" {
		t.Fatalf("changes = %+v", rep.FactChanges)
	}
}

func TestAdminIsLANOnly(t *testing.T) {
	for path, want := range map[string]bool{"lan": true, "tunnel": false, "direct": false, "": false} {
		if adminPathOK(path) != want {
			t.Errorf("adminPathOK(%q) = %v, want %v", path, !want, want)
		}
	}
}

func TestPathLabel(t *testing.T) {
	mk := func(remote, kind string) *http.Request {
		r := httptest.NewRequest("GET", "/ws", nil)
		r.RemoteAddr = remote
		return r.WithContext(context.WithValue(r.Context(), listenerKindKey{}, kind))
	}
	cases := []struct{ remote, kind, want string }{
		{"192.168.1.20:5000", "", "lan"},
		{"127.0.0.1:5000", "", "lan"},
		{"203.0.113.9:5000", "", "direct"},
		{"127.0.0.1:5000", "tunnel", "tunnel"}, // tunnelled clients look like loopback; the listener wins
		{"192.168.1.20:5000", "tunnel", "tunnel"},
	}
	for _, c := range cases {
		if got := pathLabel(mk(c.remote, c.kind)); got != c.want {
			t.Errorf("pathLabel(%s,%q) = %s, want %s", c.remote, c.kind, got, c.want)
		}
	}
}

func TestNoListFSRefusesDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := noListFS{http.Dir(dir)}
	if _, err := fs.Open("/"); err == nil {
		t.Fatal("directory opened; listing would be exposed")
	}
	f, err := fs.Open("/a.bin")
	if err != nil {
		t.Fatalf("file refused: %v", err)
	}
	f.Close()
}
