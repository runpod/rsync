//go:build !windows

// Package receiverhardening_test drives the daemon receiver with a real rsync
// client over destinations that are awkward on purpose: names at the limit of
// what a filesystem takes, files that are already there, and entries that are
// not what the sender's file list says they are.
package receiverhardening_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/runpod/rsync/internal/rsynctest"
	"github.com/runpod/rsync/internal/testlogger"
	"github.com/runpod/rsync/rsyncd"
)

// push runs rsync from src into a daemon serving dest, and fails the test
// unless both ends report success.
func push(t *testing.T, src, dest string, extraArgs ...string) {
	t.Helper()
	rsyncBin := rsynctest.TridgeOrGTFO(t, "needs an rsync client that speaks the daemon protocol")

	resolve := func(context.Context, string, string) (rsyncd.Module, error) {
		return rsyncd.Module{Name: "vol", Path: dest, Writable: true}, nil
	}
	srv, err := rsyncd.NewServer(nil,
		rsyncd.WithModuleResolver(resolve),
		rsyncd.WithStderr(testlogger.New(t)),
		rsyncd.DontRestrict(),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	served := make(chan error, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			served <- acceptErr
			return
		}
		defer conn.Close()
		served <- srv.HandleDaemonConn(context.Background(), rsyncd.NewConnection(conn, conn, conn.RemoteAddr().String()))
	}()

	args := append([]string{"-a"}, extraArgs...)
	args = append(args, src+"/", "rsync://"+ln.Addr().String()+"/vol/")
	// A deadline on the client too: a hang is the very failure these tests
	// guard against, and it should fail here, not at go test's own timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, clientErr := exec.CommandContext(ctx, rsyncBin, args...).CombinedOutput()
	select {
	case daemonErr := <-served:
		if clientErr != nil || daemonErr != nil {
			t.Fatalf("rsync %v: client: %v, daemon: %v\n%s", args, clientErr, daemonErr, out)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon never finished the connection; client: %v\n%s", clientErr, out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
	}
}

// The receiver writes each file to a temporary named after it. That name must
// stay legal however long the file's own name is.
func TestLongFileNames(t *testing.T) {
	tests := []struct {
		name string
		len  int
	}{
		{name: "well under the limit", len: 100},
		{name: "longest name whose temporary used to fit", len: 235},
		{name: "just past where the temporary overflowed", len: 236},
		{name: "NAME_MAX", len: 255},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			src, dest := filepath.Join(tmp, "src"), filepath.Join(tmp, "dest")
			fileName := strings.Repeat("n", tt.len)
			write(t, filepath.Join(src, "d", fileName), "payload")

			push(t, src, dest)
			requireContent(t, filepath.Join(dest, "d", fileName), "payload")
			entries, err := os.ReadDir(filepath.Join(dest, "d"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("temporaries left beside the file: %v", entries)
			}
		})
	}
}

// -a preserves times, and writing a directory's entries moves its mtime, so
// directories have to get theirs back once the files are in.
func TestDirectoryTimesSurviveTheirContents(t *testing.T) {
	tmp := t.TempDir()
	src, dest := filepath.Join(tmp, "src"), filepath.Join(tmp, "dest")
	write(t, filepath.Join(src, "d", "sub", "f"), "payload")
	write(t, filepath.Join(src, "d", "g"), "payload")
	then := time.Unix(1_000_000_000, 0)
	for _, dir := range []string{"d/sub", "d", "."} {
		if err := os.Chtimes(filepath.Join(src, dir), then, then); err != nil {
			t.Fatal(err)
		}
	}

	push(t, src, dest)
	for _, dir := range []string{"d/sub", "d", "."} {
		st, err := os.Stat(filepath.Join(dest, dir))
		if err != nil {
			t.Fatal(err)
		}
		if !st.ModTime().Equal(then) {
			t.Errorf("%s: mtime = %v, want %v", dir, st.ModTime(), then)
		}
	}
}

// --whole-file is what rsync uses between two local paths. A daemon that
// refuses the flag fails the transfer outright.
func TestWholeFile(t *testing.T) {
	for _, flag := range []string{"--whole-file", "-W"} {
		t.Run(flag, func(t *testing.T) {
			tmp := t.TempDir()
			src, dest := filepath.Join(tmp, "src"), filepath.Join(tmp, "dest")
			write(t, filepath.Join(src, "changed"), "the new contents, longer than the old")
			write(t, filepath.Join(src, "new"), "new")
			// Already there with other contents and another mtime: without the
			// flag this file goes through the delta algorithm.
			write(t, filepath.Join(dest, "changed"), "old")
			old := time.Unix(1_000_000_000, 0)
			if err := os.Chtimes(filepath.Join(dest, "changed"), old, old); err != nil {
				t.Fatal(err)
			}

			push(t, src, dest, flag)
			requireContent(t, filepath.Join(dest, "changed"), "the new contents, longer than the old")
			requireContent(t, filepath.Join(dest, "new"), "new")
		})
	}
}

// --whole-file means no delta basis, a retained partial included: the file
// arrives whole and the partial does not outlive it.
func TestWholeFileWinsOverAPartialResume(t *testing.T) {
	tmp := t.TempDir()
	src, dest := filepath.Join(tmp, "src"), filepath.Join(tmp, "dest")
	// The partial is a multi-block prefix of the source, so a generator that
	// sent sums from it would get blocks matched, and the receiver, which opens
	// no basis under --whole-file, would then fail the transfer.
	content := strings.Repeat("0123456789abcdef", 512) // 8 KiB
	write(t, filepath.Join(src, "f"), content)
	write(t, filepath.Join(dest, "f.partial"), content[:4096])

	push(t, src, dest, "--partial", "--whole-file")
	requireContent(t, filepath.Join(dest, "f"), content)
	if _, err := os.Lstat(filepath.Join(dest, "f.partial")); !os.IsNotExist(err) {
		t.Errorf("f.partial is still there: %v", err)
	}
}

// -a is --devices and --specials together. A client that turns devices off
// must still get its FIFOs across.
func TestSpecialsWithoutDevices(t *testing.T) {
	tmp := t.TempDir()
	src, dest := filepath.Join(tmp, "src"), filepath.Join(tmp, "dest")
	write(t, filepath.Join(src, "f"), "payload")
	if err := syscall.Mkfifo(filepath.Join(src, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}

	push(t, src, dest, "--no-devices")
	st, err := os.Lstat(filepath.Join(dest, "fifo"))
	if err != nil || st.Mode().Type() != os.ModeNamedPipe {
		t.Fatalf("fifo = %v, %v; want a FIFO", st, err)
	}
}

// Under --partial the receiver writes to "<name>.partial.tmp". A name too long
// for that suffix must still transfer.
func TestLongFileNamesWithPartial(t *testing.T) {
	tmp := t.TempDir()
	src, dest := filepath.Join(tmp, "src"), filepath.Join(tmp, "dest")
	short, long := strings.Repeat("s", 100), strings.Repeat("n", 250)
	write(t, filepath.Join(src, short), "payload")
	write(t, filepath.Join(src, long), "payload")

	push(t, src, dest, "--partial")
	requireContent(t, filepath.Join(dest, short), "payload")
	requireContent(t, filepath.Join(dest, long), "payload")
}

// What is at a file's name in the destination, or at the name of the temporary
// --partial writes to, is the tree owner's choice. The transfer must replace it
// without opening it: opening a FIFO blocks for good.
func TestNonRegularEntriesAreReplacedNotOpened(t *testing.T) {
	fifo := func(t *testing.T, path string) {
		if err := syscall.Mkfifo(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	symlinkToFIFO := func(t *testing.T, path string) {
		fifo(t, path+".fifo")
		if err := os.Symlink(filepath.Base(path)+".fifo", path); err != nil {
			t.Fatal(err)
		}
	}
	dangling := func(t *testing.T, path string) {
		if err := os.Symlink("nowhere", path); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		// at is the name planted, beside the file "f" the source sends.
		at    string
		plant func(t *testing.T, path string)
		args  []string
	}{
		{name: "FIFO at the file's name", at: "f", plant: fifo},
		{name: "symlink to a FIFO at the file's name", at: "f", plant: symlinkToFIFO},
		{name: "dangling symlink at the file's name", at: "f", plant: dangling},
		{name: "FIFO at the partial temporary's name", at: "f.partial.tmp", plant: fifo, args: []string{"--partial"}},
		{name: "symlink to a FIFO at the partial temporary's name", at: "f.partial.tmp", plant: symlinkToFIFO, args: []string{"--partial"}},
		{name: "FIFO at the partial's name", at: "f.partial", plant: fifo, args: []string{"--partial"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			src, dest := filepath.Join(tmp, "src"), filepath.Join(tmp, "dest")
			write(t, filepath.Join(src, "f"), "payload")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			tt.plant(t, filepath.Join(dest, tt.at))

			push(t, src, dest, tt.args...)
			st, err := os.Lstat(filepath.Join(dest, "f"))
			if err != nil || !st.Mode().IsRegular() {
				t.Fatalf("f = %v, %v; want a regular file", st, err)
			}
			requireContent(t, filepath.Join(dest, "f"), "payload")
		})
	}
}
