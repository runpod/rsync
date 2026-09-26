package rsyncd_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/runpod/rsync/internal/rsynctest"
	"github.com/runpod/rsync/internal/testlogger"
	"github.com/runpod/rsync/rsyncd"
)

// These tests cover serving a module whose path is computed when the client
// connects, which lets a caller route each connection to a different directory
// without knowing the set of directories at startup.

// migrationRegistry stands in for a caller that maps an identifier supplied by
// the client to a directory, and pins each identifier to an expected peer.
type migrationRegistry struct {
	dir        map[string]string
	sourceAddr map[string]string
}

func (r *migrationRegistry) resolve(remoteAddr, requestedModule string) (rsyncd.Module, error) {
	dir, ok := r.dir[requestedModule]
	if !ok {
		return rsyncd.Module{}, fmt.Errorf("unknown migration %q", requestedModule)
	}

	if want, pinned := r.sourceAddr[requestedModule]; pinned {
		host, _, err := net.SplitHostPort(remoteAddr)
		if err != nil {
			return rsyncd.Module{}, fmt.Errorf("bad remote address %q: %w", remoteAddr, err)
		}
		if host != want {
			return rsyncd.Module{}, fmt.Errorf("migration %q is not authorised for %s", requestedModule, host)
		}
	}

	return rsyncd.Module{Name: requestedModule, Path: dir, Writable: true}, nil
}

// serveResolved runs the daemon over its own accept loop rather than Serve, so
// each connection's outcome is observable. Outcomes arrive on the returned
// channel, one per connection. It has to be a channel rather than a map the
// test reads at the end: the daemon writes its refusal to the client before
// returning, so the client can exit while the outcome is still in flight.
func serveResolved(t *testing.T, resolve rsyncd.ModuleResolver) (port string, outcomes <-chan error) {
	t.Helper()

	srv, err := rsyncd.NewServer(nil,
		rsyncd.WithModuleResolver(resolve),
		rsyncd.WithStderr(os.Stderr),
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

	// Buffered past the number of connections any test makes, so a handler
	// never blocks on a test that is not reading.
	served := make(chan error, 8)
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				remoteAddr := conn.RemoteAddr().String()
				c := rsyncd.NewConnection(conn, conn, remoteAddr)
				served <- srv.HandleDaemonConn(ctx, c)
			}()
		}
	}()

	_, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	return port, served
}

// awaitOutcome returns the next connection outcome, failing the test rather
// than hanging if the daemon never reports one.
func awaitOutcome(t *testing.T, outcomes <-chan error) error {
	t.Helper()

	select {
	case err := <-outcomes:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the daemon to report a connection outcome")
		return nil
	}
}

func writeTree(t *testing.T, root, marker string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "marker"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
}

// push sends the contents of srcDir to a module path on the daemon. The source
// is named relative to cmd.Dir rather than absolutely: Windows CI uses Cygwin
// rsync, which reads the colon in "C:\..." as a host separator and refuses the
// command line as two remotes. The port goes in a flag for the same reason.
func push(t *testing.T, rsyncBin, port, srcDir, modulePath string) error {
	t.Helper()

	cmd := exec.Command(rsyncBin,
		"--archive",
		"--port="+port,
		filepath.Base(srcDir)+"/",
		"rsync://localhost/"+modulePath)
	cmd.Dir = filepath.Dir(srcDir)
	cmd.Env = append(os.Environ(), "LANG=C.UTF-8")
	cmd.Stdout = testlogger.New(t)
	cmd.Stderr = testlogger.New(t)
	return cmd.Run()
}

// TestModuleResolverRoutesConcurrentConnections is the core check: two clients
// naming different migrations are served different roots at the same time, and
// neither tree leaks into the other.
func TestModuleResolverRoutesConcurrentConnections(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	reg := &migrationRegistry{dir: map[string]string{}}

	type migration struct{ id, src, dst string }
	migrations := []migration{
		{id: "migration-aaa", src: filepath.Join(tmp, "src-aaa"), dst: filepath.Join(tmp, "vol-aaa")},
		{id: "migration-bbb", src: filepath.Join(tmp, "src-bbb"), dst: filepath.Join(tmp, "vol-bbb")},
	}
	for _, m := range migrations {
		writeTree(t, m.src, m.id)
		if err := os.MkdirAll(m.dst, 0o755); err != nil {
			t.Fatal(err)
		}
		reg.dir[m.id] = m.dst
	}

	port, _ := serveResolved(t, reg.resolve)

	var wg sync.WaitGroup
	errs := make([]error, len(migrations))
	for i, m := range migrations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = push(t, rsyncBin, port, m.src, m.id+"/")
		}()
	}
	wg.Wait()

	for i, m := range migrations {
		if errs[i] != nil {
			t.Fatalf("%s: push failed: %v", m.id, errs[i])
		}

		got, err := os.ReadFile(filepath.Join(m.dst, "data", "marker"))
		if err != nil {
			t.Fatalf("%s: %v", m.id, err)
		}
		if string(got) != m.id {
			t.Errorf("%s: landed in the wrong root: marker = %q", m.id, got)
		}
	}
}

// TestModuleResolverConfinesToResolvedRoot checks the isolation property that a
// shared root cannot offer: a client asking for another migration's directory
// stays inside the root it was given, because that root is its whole view.
func TestModuleResolverConfinesToResolvedRoot(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	mine := filepath.Join(tmp, "vol-mine")
	victim := filepath.Join(tmp, "vol-victim")
	writeTree(t, src, "attacker")
	for _, d := range []string{mine, victim} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	reg := &migrationRegistry{dir: map[string]string{
		"migration-mine":   mine,
		"migration-victim": victim,
	}}
	port, _ := serveResolved(t, reg.resolve)

	// Authorised for one migration, aiming at the other migration's directory name.
	err := push(t, rsyncBin, port, src, "migration-mine/vol-victim/")
	if err != nil {
		t.Fatalf("push failed: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(victim, "data", "marker")); !os.IsNotExist(statErr) {
		t.Fatalf("write reached the other migration's directory: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(mine, "vol-victim", "data", "marker")); statErr != nil {
		t.Fatalf("write should have stayed inside the resolved root: %v", statErr)
	}
}

// TestModuleResolverRejectsUnknownAndUnauthorised covers the resolver refusing a
// connection, and confirms the refusal surfaces as a per-connection error.
func TestModuleResolverRejectsUnknownAndUnauthorised(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "vol")
	writeTree(t, src, "data")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := &migrationRegistry{
		dir:        map[string]string{"migration-known": dst, "migration-elsewhere": dst},
		sourceAddr: map[string]string{"migration-elsewhere": "192.0.2.1"},
	}
	port, outcomes := serveResolved(t, reg.resolve)

	for _, tc := range []struct{ name, module string }{
		{"unknown migration", "migration-unknown"},
		{"migration pinned to a different source", "migration-elsewhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := push(t, rsyncBin, port, src, tc.module+"/"); err == nil {
				t.Fatal("expected the client to fail")
			}
			if _, statErr := os.Stat(filepath.Join(dst, "data", "marker")); !os.IsNotExist(statErr) {
				t.Fatalf("rejected connection still wrote data: %v", statErr)
			}

			// The refusal must be reported, not silently dropped, so a caller
			// can tell a refusal from a completed transfer.
			if err := awaitOutcome(t, outcomes); err == nil {
				t.Error("connection reported success despite being refused")
			}
		})
	}
}
