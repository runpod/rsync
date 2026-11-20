//go:build linux || darwin

package receiver

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The owner of the destination tree picks what is at a name by the time it is
// opened. Only a regular file may be opened, and nothing may block the open.
func TestOpenRegular(t *testing.T) {
	const (
		wantFile       = iota // the file's handle
		wantNone              // no handle: not a regular file
		wantNotExist          // an error
		wantFileOrNone        // os.Root follows an in-root symlink on some platforms
	)
	tests := []struct {
		name string
		make func(t *testing.T, dir string)
		want int
	}{
		{
			name: "regular file is opened",
			make: func(t *testing.T, dir string) { must(t, os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0o644)) },
			want: wantFile,
		},
		{
			name: "FIFO is not opened and does not block",
			make: func(t *testing.T, dir string) { must(t, syscall.Mkfifo(filepath.Join(dir, "f"), 0o644)) },
			want: wantNone,
		},
		{
			name: "directory is not a regular file",
			make: func(t *testing.T, dir string) { must(t, os.Mkdir(filepath.Join(dir, "f"), 0o755)) },
			want: wantNone,
		},
		{
			name: "symlink to a regular file yields that file or nothing",
			make: func(t *testing.T, dir string) {
				must(t, os.WriteFile(filepath.Join(dir, "target"), []byte("data"), 0o644))
				must(t, os.Symlink("target", filepath.Join(dir, "f")))
			},
			want: wantFileOrNone,
		},
		{
			name: "symlink to a FIFO does not block",
			make: func(t *testing.T, dir string) {
				must(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))
				must(t, os.Symlink("fifo", filepath.Join(dir, "f")))
			},
			want: wantNone,
		},
		{
			name: "missing name is an error",
			make: func(*testing.T, string) {},
			want: wantNotExist,
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
				if st, err := got.f.Stat(); err != nil || !st.Mode().IsRegular() {
					t.Fatalf("opened something that is not a regular file: %v, %v", st, err)
				}
			}

			switch tt.want {
			case wantFile:
				if got.err != nil || got.f == nil {
					t.Fatalf("openRegular = %v, %v; want a file", got.f, got.err)
				}
				b := make([]byte, 4)
				if _, err := got.f.Read(b); err != nil || string(b) != "data" {
					t.Fatalf("read %q, %v; want the file's contents", b, err)
				}
			case wantNone:
				if got.f != nil {
					t.Fatalf("opened %s, want no file", got.f.Name())
				}
			case wantNotExist:
				if !os.IsNotExist(got.err) {
					t.Fatalf("err = %v, want not-exist", got.err)
				}
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
