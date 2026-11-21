//go:build !windows

package receiver

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runpod/rsync"
	"github.com/runpod/rsync/internal/log"
	"github.com/runpod/rsync/internal/rsyncopts"
)

// The tree's owner can remove or replace a directory after the generator made
// it. The touch-up at the end must skip that one and still finish the rest.
// Not on Windows, where a directory has no permission bits to check.
func TestTouchUpDirsSkipsWhatIsNoLongerADirectory(t *testing.T) {
	then := time.Unix(1_000_000_000, 0)
	tests := []struct {
		name string
		// list is the directories transferred; a, b and c when empty.
		list []string
		// change is what happens to directory "b" before the touch-up.
		change func(t *testing.T, dest string)
	}{
		{name: "nothing changed", change: func(*testing.T, string) {}},
		{name: "directory was removed", change: func(t *testing.T, dest string) {
			if err := os.Remove(filepath.Join(dest, "b")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory was replaced by a file", change: func(t *testing.T, dest string) {
			if err := os.Remove(filepath.Join(dest, "b")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dest, "b"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory above it was replaced by a file", list: []string{"a", "b", "b/sub", "c"}, change: func(t *testing.T, dest string) {
			if err := os.RemoveAll(filepath.Join(dest, "b")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dest, "b"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory was replaced by a symlink to a file", change: func(t *testing.T, dest string) {
			if err := os.Remove(filepath.Join(dest, "b")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dest, "target"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("target", filepath.Join(dest, "b")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := t.TempDir()
			var list []*File
			names := tt.list
			if names == nil {
				names = []string{"a", "b", "c"}
			}
			for _, name := range names {
				if err := os.Mkdir(filepath.Join(dest, name), 0o755); err != nil {
					t.Fatal(err)
				}
				list = append(list, &File{Name: name, Mode: rsync.S_IFDIR | 0o750, ModTime: then})
			}
			tt.change(t, dest)

			rt := newTestTransfer(t, dest)
			rt.Logger = log.New(io.Discard)
			rt.Opts = &TransferOpts{
				PreserveTimes: true,
				DebugGTE:      func(rsyncopts.DebugLevel, uint16) bool { return false },
			}
			if err := rt.touchUpDirs(list); err != nil {
				t.Fatalf("touchUpDirs: %v", err)
			}

			// The directories either side of the one that changed are done.
			for _, name := range []string{"a", "c"} {
				st, err := os.Stat(filepath.Join(dest, name))
				if err != nil {
					t.Fatal(err)
				}
				if !st.ModTime().Equal(then) || st.Mode().Perm() != 0o750 {
					t.Errorf("%s: mtime %v mode %o, want %v and 750", name, st.ModTime(), st.Mode().Perm(), then)
				}
			}
			// A directory's mode never lands on what replaced it.
			if st, err := os.Stat(filepath.Join(dest, "target")); err == nil && st.Mode().Perm() != 0o600 {
				t.Errorf("the symlink's target was chmodded to %o", st.Mode().Perm())
			}
			if st, err := os.Lstat(filepath.Join(dest, "b")); err == nil && st.Mode().IsRegular() && st.Mode().Perm() != 0o600 {
				t.Errorf("the file that replaced b was chmodded to %o", st.Mode().Perm())
			}
		})
	}
}

// Only a directory that is gone is skipped. Being unable to look at one for
// any other reason is the transfer's failure to report.
func TestTouchUpDirsReportsOtherErrors(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root is not refused by a directory's mode")
	}
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "locked", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Without search permission on "locked", "locked/sub" cannot be looked at.
	if err := os.Chmod(filepath.Join(dest, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "locked"), 0o755) })

	rt := newTestTransfer(t, dest)
	rt.Logger = log.New(io.Discard)
	rt.Opts = &TransferOpts{
		PreserveTimes: true,
		DebugGTE:      func(rsyncopts.DebugLevel, uint16) bool { return false },
	}
	err := rt.touchUpDirs([]*File{{Name: "locked/sub", Mode: rsync.S_IFDIR | 0o750, ModTime: time.Unix(1_000_000_000, 0)}})
	if !os.IsPermission(err) {
		t.Fatalf("touchUpDirs = %v, want a permission error", err)
	}
}
