package version

import (
	"runtime/debug"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.4", -1},
		{"1.3.0", "1.2.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.2", "1.2.0", 0},
		{"", "0.0.1", -1},
		{"0.1.0", "", 1},
		{"", "", 0},
		{"1.2.x", "1.2.0", 0}, // non-numeric component treated as 0
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNewer(t *testing.T) {
	if !Newer("1.1.0", "1.0.0") {
		t.Error("expected 1.1.0 newer than 1.0.0")
	}
	if Newer("1.0.0", "1.0.0") {
		t.Error("expected 1.0.0 not newer than itself")
	}
	if Newer("1.0.0", "1.1.0") {
		t.Error("expected 1.0.0 not newer than 1.1.0")
	}
}

func TestFormatBuildInfo(t *testing.T) {
	cases := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{
			name: "clean",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "77d08842b26f9221366e777bc2ccec817edb30e4"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: "77d0884",
		},
		{
			name: "dirty",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "77d08842b26f9221366e777bc2ccec817edb30e4"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "77d0884-dirty",
		},
		{
			name: "short revision left as-is",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc123"},
			},
			want: "abc123",
		},
		{
			name:     "no vcs info",
			settings: []debug.BuildSetting{{Key: "-trimpath", Value: "true"}},
			want:     "",
		},
		{
			name:     "empty",
			settings: nil,
			want:     "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatBuildInfo(c.settings); got != c.want {
				t.Errorf("formatBuildInfo(%+v) = %q, want %q", c.settings, got, c.want)
			}
		})
	}
}

// TestBuildInfoRunsCleanly confirms the real BuildInfo() (backed by the
// actual ambient debug.ReadBuildInfo()) never panics and returns something
// formatBuildInfo could have produced — empty is a valid, expected outcome
// under `go test`'s own build (see formatBuildInfo's doc comment).
func TestBuildInfoRunsCleanly(t *testing.T) {
	got := BuildInfo()
	if got != "" && len(got) < 7 {
		t.Errorf("BuildInfo() = %q, want empty or at least a 7-char hash", got)
	}
}
