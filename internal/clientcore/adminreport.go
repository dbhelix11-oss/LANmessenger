package clientcore

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"lanmessenger/internal/proto"
)

func ts(sec int64) string {
	if sec <= 0 {
		return "—"
	}
	return time.Unix(sec, 0).Format("2006-01-02 15:04:05")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// FormatDeviceReport renders the relay's record of one device as plain text.
func FormatDeviceReport(d proto.DeviceReport) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("Name            %s", d.Entry.DisplayName)
	w("Device ID       %s", d.Entry.DeviceID)
	w("Fingerprint     %s", d.Fingerprint)
	w("State           %s%s", d.Entry.State, map[bool]string{true: "  (online now)", false: ""}[d.Online])
	w("Enrolled        %s", ts(d.CreatedAt))
	w("Last approved   %s", ts(d.ApprovedAt))
	if d.RevokedAt > 0 {
		w("Revoked         %s", ts(d.RevokedAt))
		w("Return attempts %d (last %s, match on %s)", d.RevokeAttempts, ts(d.LastRevokeAttempt), orDash(d.LastRevokeMatch))
		if d.LastRevokeSignals != "" {
			w("                last by %q, resembling by: %s", d.LastRevokeName, d.LastRevokeSignals)
		}
		w("                'key' = same signing key re-registered; 'id' = only the device ID signed in;")
		w("                'fingerprint' = a new key from a connection like this one (tls, client, address): a hint only")
	}
	w("Connections     %d recorded", d.ConnCount)
	if len(d.Conns) > 0 {
		c := d.Conns[0]
		w("Client          %s", orDash(c.ClientKind))
		osName, arch := c.ClientOS, c.ClientArch
		if osName == "" && d.Facts != nil { // older builds declare it only in signed facts
			osName, arch = d.Facts.OS, d.Facts.Arch
		}
		w("Operating sys   %s / %s", orDash(osName), orDash(arch))
	} else {
		w("Client          — (no connection recorded yet)")
	}
	w("Paths seen      %s", orDash(strings.Join(d.Paths, ", ")))
	w("TLS hashes seen %s", orDash(strings.Join(d.TLSHashes, ", ")))
	if len(d.TLSHashes) > 1 {
		w("                ⚠ more than one TLS handshake shape: more than one program or build has connected")
	}
	w("")
	if d.Facts == nil {
		w("Declared facts  (none received yet)")
	} else {
		sig := "signed by the device key"
		if !d.FactsVerified {
			sig = "NOT verified against the device key"
		}
		w("Declared facts  %s, updated %s", sig, ts(d.FactsAt))
		w("  version      %s", orDash(d.Facts.Version))
		w("  system       %s / %s", orDash(d.Facts.OS), orDash(d.Facts.Arch))
		w("  build hash   %s", orDash(d.Facts.BuildHash))
		w("  model        %s", orDash(d.Facts.Model))
		w("  time zone    %s", orDash(d.Facts.TZ))
	}
	if len(d.FactChanges) > 0 {
		w("")
		w("Changes in declared facts (newest first)")
		for _, c := range d.FactChanges {
			w("  %s  %s: %q → %q", ts(c.TS), c.Field, c.Old, c.New)
		}
	}
	w("")
	w("Recent connections (newest first)")
	if len(d.Conns) == 0 {
		w("  (none)")
	}
	for _, c := range d.Conns {
		end := "connected"
		if c.DisconnectedAt > 0 {
			end = "for " + (time.Duration(c.DisconnectedAt-c.ConnectedAt) * time.Second).String()
		}
		w("  %s  %-6s %s  tls=%s  v%s  rtt avg/min/max %d/%d/%d ms  %s",
			ts(c.ConnectedAt), c.Path, c.RemoteHost, orDash(c.TLSHash), orDash(c.ClientVersion),
			c.RTTAvgMs, c.RTTMinMs, c.RTTMaxMs, end)
	}
	w("")
	w("Notes: 'tunnel' clients all appear to come from 127.0.0.1; the relay cannot see their real address.")
	w("Declared facts and TLS hashes are advisory signals for spotting change, not proof of identity.")
	return b.String()
}

// DeviceMatches returns the reports whose display name (case-insensitive) or
// device id (prefix) equals / starts with q.
func DeviceMatches(reps []proto.DeviceReport, q string) []proto.DeviceReport {
	q = strings.ToLower(strings.TrimSpace(q))
	var out []proto.DeviceReport
	for _, r := range reps {
		if q != "" && (strings.ToLower(r.Entry.DisplayName) == q || strings.HasPrefix(r.Entry.DeviceID, q)) {
			out = append(out, r)
		}
	}
	return out
}

// DeviceTable renders one line per device for a terminal.
func DeviceTable(reps []proto.DeviceReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-18s %-9s %-7s %-14s %-18s %s\n", "NAME", "STATE", "ONLINE", "VIA", "CLIENT", "FINGERPRINT")
	for _, r := range reps {
		on, via, what := "no", "—", "—"
		if r.Online {
			on = "yes"
		}
		if len(r.Conns) > 0 {
			c := r.Conns[0]
			via = orDash(c.Path)
			what = strings.TrimSpace(c.ClientKind + " " + c.ClientOS)
			if what == "" && r.Facts != nil {
				what = r.Facts.OS
			}
			what = orDash(what)
		}
		fmt.Fprintf(&b, "%-18s %-9s %-7s %-14s %-18s %s\n",
			clipRunes(r.Entry.DisplayName, 18), r.Entry.State, on, via, clipRunes(what, 18), r.Fingerprint)
	}
	return b.String()
}

// DeviceStats summarises the whole household for `admin stats`.
func DeviceStats(reps []proto.DeviceReport) string {
	states := map[string]int{}
	paths := map[string]int{}
	online, multiTLS, noFacts := 0, 0, 0
	for _, r := range reps {
		states[string(r.Entry.State)]++
		if r.Online {
			online++
		}
		for _, p := range r.Paths {
			paths[p]++
		}
		if len(r.TLSHashes) > 1 {
			multiTLS++
		}
		if r.Facts == nil {
			noFacts++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Devices            %d (%d online now)\n", len(reps), online)
	fmt.Fprintf(&b, "By state           %s\n", countList(states))
	fmt.Fprintf(&b, "Devices seen on    %s\n", countList(paths))
	fmt.Fprintf(&b, "No declared facts  %d\n", noFacts)
	fmt.Fprintf(&b, "Several TLS shapes %d (more than one program or build has connected)\n", multiTLS)
	return b.String()
}

func countList(m map[string]int) string {
	if len(m) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", orDash(k), m[k])
	}
	return strings.Join(parts, ", ")
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
