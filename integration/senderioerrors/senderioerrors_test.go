// Package senderioerrors_test covers what the daemon reports when a sender
// could not read part of what it was asked to send. The session still ends
// cleanly in that case, so the outcome is all a destination has to go on.
package senderioerrors_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/runpod/rsync/internal/rsynctest"
	"github.com/runpod/rsync/internal/testlogger"
	"github.com/runpod/rsync/rsyncd"
)

// serveOneModule serves dest under moduleName and reports each connection's
// outcome. Awaited rather than sampled: the client can exit while the daemon
// is still finishing up.
func serveOneModule(t *testing.T, moduleName, dest string) (port string, outcomes <-chan error) {
	t.Helper()

	resolve := func(context.Context, string, string) (rsyncd.Module, error) {
		return rsyncd.Module{Name: moduleName, Path: dest, Writable: true}, nil
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
	t.Cleanup(func() { _ = ln.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	served := make(chan error, 4)
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				served <- srv.HandleDaemonConn(ctx, rsyncd.NewConnection(conn, conn, conn.RemoteAddr().String()))
			}()
		}
	}()

	_, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	return port, served
}

// requireUnreadableWorks skips where mode 000 does not actually deny a read,
// which would leave the sender with nothing to report.
func requireUnreadableWorks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions do not gate reads on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks, so the file would still be readable")
	}
}

func awaitOutcome(t *testing.T, outcomes <-chan error) error {
	t.Helper()
	select {
	case err := <-outcomes:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("daemon never reported a connection outcome")
		return nil
	}
}

func push(t *testing.T, rsyncBin, workDir, sourceName, port, module string) error {
	t.Helper()
	cmd := exec.Command(rsyncBin, "--archive", "--port="+port, sourceName+"/", "rsync://localhost/"+module+"/")
	cmd.Dir = workDir
	cmd.Stdout = testlogger.New(t)
	cmd.Stderr = testlogger.New(t)
	return cmd.Run()
}

// The common shape of this: the sender skips the file it cannot read, exits 23,
// and everything else arrives intact.
func TestDaemonReportsSenderIOErrors(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")
	requireUnreadableWorks(t)

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	dest := filepath.Join(tmp, "dest")
	for _, dir := range []string{source, dest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "readable"), []byte("arrives"), 0o644); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(source, "unreadable")
	if err := os.WriteFile(unreadable, []byte("does not"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}

	port, outcomes := serveOneModule(t, "mod", dest)

	// The client is expected to fail; under test is whether the daemon agrees.
	if err := push(t, rsyncBin, tmp, "source", port, "mod"); err == nil {
		t.Fatal("rsync client succeeded; expected it to report a partial transfer")
	}

	outcome := awaitOutcome(t, outcomes)
	if !errors.Is(outcome, rsyncd.ErrSenderIOErrors) {
		t.Errorf("daemon outcome = %v, want it to wrap ErrSenderIOErrors", outcome)
	}

	// We report rather than abort, so a caller can retry against a partial tree.
	got, err := os.ReadFile(filepath.Join(dest, "readable"))
	if err != nil {
		t.Fatalf("readable file should still have been written: %v", err)
	}
	if string(got) != "arrives" {
		t.Errorf("readable = %q, want %q", got, "arrives")
	}
	if _, err := os.Stat(filepath.Join(dest, "unreadable")); !os.IsNotExist(err) {
		t.Errorf("unreadable file should be absent, stat err = %v", err)
	}
}

// The other half of the detection. An unreadable directory fails the sender
// while it builds the file list, so nothing inside is ever requested and only
// the count it reports up front shows anything went wrong.
func TestDaemonReportsSenderIOErrorsFromUnreadableDirectory(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")
	requireUnreadableWorks(t)

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	dest := filepath.Join(tmp, "dest")
	locked := filepath.Join(source, "locked")
	for _, dir := range []string{source, dest, locked} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "readable"), []byte("arrives"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "hidden"), []byte("never listed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	// t.TempDir cleanup cannot recurse into it otherwise.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	port, outcomes := serveOneModule(t, "mod", dest)

	if err := push(t, rsyncBin, tmp, "source", port, "mod"); err == nil {
		t.Fatal("rsync client succeeded; expected it to report a partial transfer")
	}
	if outcome := awaitOutcome(t, outcomes); !errors.Is(outcome, rsyncd.ErrSenderIOErrors) {
		t.Errorf("daemon outcome = %v, want it to wrap ErrSenderIOErrors", outcome)
	}
}

// The error has to stay specific to a sender that lost data. The repeat runs
// are the ones at risk: a file the destination already has is never requested.
func TestDaemonReportsNoErrorWhenSenderReadsEverything(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	dest := filepath.Join(tmp, "dest")
	for _, dir := range []string{source, dest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "readable"), []byte("arrives"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub", "nested"), []byte("also arrives"), 0o644); err != nil {
		t.Fatal(err)
	}

	port, outcomes := serveOneModule(t, "mod", dest)

	for _, run := range []struct {
		name   string
		before func()
	}{
		{name: "first copy", before: func() {}},
		{name: "repeat with nothing to do", before: func() {}},
		{name: "repeat after a change, so one file goes by delta", before: func() {
			if err := os.WriteFile(filepath.Join(source, "readable"), []byte("arrives, edited"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		run.before()
		if err := push(t, rsyncBin, tmp, "source", port, "mod"); err != nil {
			t.Fatalf("%s: rsync: %v", run.name, err)
		}
		if outcome := awaitOutcome(t, outcomes); outcome != nil {
			t.Errorf("%s: daemon outcome = %v, want nil", run.name, outcome)
		}
	}

	got, err := os.ReadFile(filepath.Join(dest, "readable"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "arrives, edited" {
		t.Errorf("readable = %q, want the edited contents", got)
	}
}
