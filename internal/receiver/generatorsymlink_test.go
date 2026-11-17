//go:build linux || darwin

package receiver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSymlink(t *testing.T) {
	tests := []struct {
		name string
		// existing is what is at the link's name beforehand; "" for nothing.
		existing string
		link     string
		target   string
	}{
		{name: "creates a new symlink", link: "l", target: "a"},
		{name: "replaces a symlink with another target", existing: "old", link: "l", target: "new"},
		{name: "replaces a symlink in a subdirectory", existing: "old", link: "sub/l", target: "new"},
		{name: "replaces a symlink whose name is NAME_MAX long", existing: "old", link: strings.Repeat("n", 255), target: "new"},
		{name: "keeps a target that points outside the root verbatim", existing: "old", link: "l", target: "/etc/passwd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(tt.link)), 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.existing != "" {
				if err := os.Symlink(tt.existing, filepath.Join(dir, tt.link)); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()

			if err := symlink(root, tt.target, tt.link); err != nil {
				t.Fatal(err)
			}
			got, err := os.Readlink(filepath.Join(dir, tt.link))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.target {
				t.Errorf("link target = %q, want %q", got, tt.target)
			}
			entries, err := os.ReadDir(filepath.Join(dir, filepath.Dir(tt.link)))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("temporaries left behind: %v", entries)
			}
		})
	}
}

// A receiver's root can be opened on a directory that whoever owns the tree
// then swaps for a symlink. Replacing a symlink must still happen in the
// directory the root holds, and nowhere the path now leads.
func TestSymlinkReplaceStaysInRootAfterItsPathIsSwapped(t *testing.T) {
	tmp := t.TempDir()
	dest, moved, outside := filepath.Join(tmp, "dest"), filepath.Join(tmp, "moved"), filepath.Join(tmp, "outside")
	for _, d := range []string{dest, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("old", filepath.Join(dest, "l")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	before, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dest, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dest); err != nil {
		t.Fatal(err)
	}

	if err := symlink(root, "new", "l"); err != nil {
		t.Fatal(err)
	}

	if got, err := os.Readlink(filepath.Join(moved, "l")); err != nil || got != "new" {
		t.Errorf("link in the root = %q, %v; want %q", got, err, "new")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("entries created outside the root: %v", entries)
	}
	after, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the directory outside the root was written to")
	}
}
