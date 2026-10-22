package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in                  string
		major, minor, patch int
		ok                  bool
	}{
		{"v1.2.3", 1, 2, 3, true},
		{"1.2.3", 1, 2, 3, true},
		{"v0.4.0-test", 0, 4, 0, true},
		{"v1.2.3-5-g75df0f1", 1, 2, 3, true},
		{"v2.0", 2, 0, 0, true},
		{"dev", 0, 0, 0, false},
		{"main", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	}
	for _, c := range cases {
		major, minor, patch, ok := parseVersion(c.in)
		if ok != c.ok || major != c.major || minor != c.minor || patch != c.patch {
			t.Errorf("parseVersion(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				c.in, major, minor, patch, ok, c.major, c.minor, c.patch, c.ok)
		}
	}
}

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.4", "v1.2.3", true},
		{"v1.3.0", "v1.2.9", true},
		{"v2.0.0", "v1.9.9", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.4", false},
		{"v0.4.0", "v0.4.0-test", false}, // same base version
		{"v2.0.0", "dev", true},          // unknown current → always update
		{"dev", "v1.0.0", true},          // unknown latest → allow update
	}
	for _, c := range cases {
		if got := isNewerVersion(c.latest, c.current); got != c.want {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestReplaceBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cert")
	if err := os.WriteFile(target, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "new-binary")
	if err := os.WriteFile(source, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(source, target); err != nil {
		t.Fatalf("replaceBinary failed: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Errorf("target not replaced: %q (%v)", data, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode()&0111 == 0 {
		t.Errorf("replaced binary must stay executable: %v", info)
	}
}
