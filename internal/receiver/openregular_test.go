//go:build linux || darwin

package receiver

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// The owner of the destination tree picks what is at a name by the time it is
// opened. Only a regular file may be opened, and nothing may block the open.
func TestOpenRegular(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T, dir string)
		// wantFile: a handle comes back. wantErr: an error does. Neither: the
		// name is reported as not a regular file. regularOrNothing: either a
		// regular file's handle or none, depending on whether the platform's
		// os.Root follows a symlink that stays inside the root.
		wantFile, wantErr, regularOrNothing bool
	}{
		{
			name:     "regular file is opened",
			make:     func(t *testing.T, dir string) { must(t, os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0o644)) },
			wantFile: true,
		},
		{
			name: "FIFO is not opened and does not block",
			make: func(t *testing.T, dir string) { must(t, syscall.Mkfifo(filepath.Join(dir, "f"), 0o644)) },
		},
		{
			name: "directory is not a regular file",
			make: func(t *testing.T, dir string) { must(t, os.Mkdir(filepath.Join(dir, "f"), 0o755)) },
		},
		{
			name: "symlink to a regular file yields that file or nothing",
			make: func(t *testing.T, dir string) {
				must(t, os.WriteFile(filepath.Join(dir, "target"), []byte("data"), 0o644))
				must(t, os.Symlink("target", filepath.Join(dir, "f")))
			},
			regularOrNothing: true,
		},
		{
			name: "symlink to a FIFO does not block",
			make: func(t *testing.T, dir string) {
				must(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))
				must(t, os.Symlink("fifo", filepath.Join(dir, "f")))
			},
		},
		{
			name:    "missing name is an error",
			make:    func(*testing.T, string) {},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.make(t, dir)
			root, err := os.OpenRoot(dir)
			must(t, err)
			defer root.Close()

			type result struct {
				f   *os.File
				err error
			}
			done := make(chan result, 1)
			go func() {
				f, _, err := openRegular(root, "f")
				done <- result{f, err}
			}()
			var got result
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("openRegular blocked")
			}
			if got.f != nil {
				defer got.f.Close()
			}

			if tt.regularOrNothing {
				if got.f != nil {
					if st, err := got.f.Stat(); err != nil || !st.Mode().IsRegular() {
						t.Fatalf("opened something that is not a regular file: %v, %v", st, err)
					}
				}
				return
			}
			if !tt.wantFile && !tt.wantErr {
				if got.f != nil {
					t.Fatalf("opened %s, want no file", got.f.Name())
				}
				return
			}
			if tt.wantErr {
				if got.err == nil || !os.IsNotExist(got.err) {
					t.Fatalf("err = %v, want not-exist", got.err)
				}
				return
			}
			if got.err != nil || got.f == nil {
				t.Fatalf("openRegular = %v, %v; want a file", got.f, got.err)
			}
			b := make([]byte, 4)
			if _, err := got.f.Read(b); err != nil || string(b) != "data" {
				t.Fatalf("read %q, %v; want the file's contents", b, err)
			}
		})
	}
}

func TestTempPrefix(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantLen int
	}{
		{name: "short name is unchanged", in: ".model.bin", wantLen: 10},
		{name: "name at the limit is unchanged", in: strings.Repeat("a", maxTempPrefix), wantLen: maxTempPrefix},
		{name: "NAME_MAX name is cut to the limit", in: "." + strings.Repeat("a", 255), wantLen: maxTempPrefix},
		// é is two bytes: a cut at an odd offset would split one.
		{name: "cut lands on a rune boundary", in: strings.Repeat("é", 127), wantLen: maxTempPrefix - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tempPrefix(tt.in)
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
			if !strings.HasPrefix(tt.in, got) || !utf8.ValidString(got) {
				t.Errorf("tempPrefix(%q) = %q: not a valid prefix", tt.in, got)
			}
			if len(got)+20 > 255 {
				t.Errorf("a %d-byte prefix leaves no room for the random suffix", len(got))
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
