package clientcore

import (
	"strings"
	"testing"

	"lanmessenger/internal/proto"
)

func sampleReports() []proto.DeviceReport {
	return []proto.DeviceReport{
		{
			Entry:       proto.DirectoryEntry{DeviceID: "aaaa1111bbbb2222", DisplayName: "Desk", State: proto.StateActive},
			Fingerprint: "AB12", Online: true, Paths: []string{"lan"}, TLSHashes: []string{"h1"},
			Conns: []proto.ConnRecord{{Path: "lan", ClientKind: "gui", ClientOS: "linux"}},
			Facts: &proto.ClientFacts{OS: "linux"},
		},
		{
			Entry:       proto.DirectoryEntry{DeviceID: "cccc3333dddd4444", DisplayName: "Phone", State: proto.StateActive},
			Fingerprint: "CD34", Paths: []string{"tunnel"}, TLSHashes: []string{"h2", "h3"},
		},
	}
}

func TestDeviceMatches(t *testing.T) {
	r := sampleReports()
	if got := DeviceMatches(r, "desk"); len(got) != 1 || got[0].Entry.DisplayName != "Desk" {
		t.Fatalf("name match: %v", got)
	}
	if got := DeviceMatches(r, "cccc"); len(got) != 1 {
		t.Fatalf("id prefix match: %v", got)
	}
	if got := DeviceMatches(r, ""); len(got) != 0 {
		t.Fatal("empty query matched")
	}
}

func TestDeviceTableAndStats(t *testing.T) {
	r := sampleReports()
	tab := DeviceTable(r)
	for _, want := range []string{"Desk", "gui linux", "lan", "Phone", "CD34"} {
		if !strings.Contains(tab, want) {
			t.Errorf("table missing %q:\n%s", want, tab)
		}
	}
	st := DeviceStats(r)
	for _, want := range []string{"2 (1 online now)", "lan 1, tunnel 1", "No declared facts  1", "Several TLS shapes 1"} {
		if !strings.Contains(st, want) {
			t.Errorf("stats missing %q:\n%s", want, st)
		}
	}
}
