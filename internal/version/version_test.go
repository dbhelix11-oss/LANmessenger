package version

import "testing"

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
