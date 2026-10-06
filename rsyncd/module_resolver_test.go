package rsyncd_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runpod/rsync/internal/rsynctest"
	"github.com/runpod/rsync/internal/testlogger"
	"github.com/runpod/rsync/rsyncd"
)

// These tests cover serving a module whose path is computed when the client
// connects, which lets a caller route each connection to a different directory
// without knowing the set of directories at startup.

// TestModuleResolverServesOneConnection is WithModuleResolver end to end with
// one server, one client, and one directory. It shares no helpers with the
// tests below so it can be read top to bottom.
func TestModuleResolverServesOneConnection(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	// A directory to send, and an empty one to receive it.
	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	dest := filepath.Join(tmp, "dest")
	for _, dir := range []string{source, dest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "hello"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Called once per connection, with the name the client asked for. That name
	// is the input: it decides which directory the client gets, and an error
	// refuses the connection. A real caller would look it up live rather than
	// from a fixed map.
	directories := map[string]string{"volume-a": dest}
	resolve := func(remoteAddr, requestedModule string) (rsyncd.Module, error) {
		path, ok := directories[requestedModule]
		if !ok {
			return rsyncd.Module{}, fmt.Errorf("unknown module %q", requestedModule)
		}
		return rsyncd.Module{
			Name:     requestedModule, // must match what was asked for
			Path:     path,
			Writable: true,
		}, nil
	}

	// nil module list: the resolver supplies the module instead of a static one.
	// DontRestrict is required because the paths are unknown until a client connects.
	srv, err := rsyncd.NewServer(nil,
		rsyncd.WithModuleResolver(resolve),
		rsyncd.DontRestrict(),
	)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, ln) }()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// "volume-a" is the module name; it is what reaches the resolver.
	rsync := exec.Command(rsyncBin,
		"--archive",
		"--port="+port,
		filepath.Base(source)+"/",
		"rsync://localhost/volume-a/")
	rsync.Dir = tmp
	rsync.Stdout = testlogger.New(t)
	rsync.Stderr = testlogger.New(t)
	if err := rsync.Run(); err != nil {
		t.Fatalf("rsync: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "world" {
		t.Errorf("hello = %q, want %q", got, "world")
	}
}

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
// each connection's outcome is observable. Outcomes must be awaited rather than
// sampled: the daemon writes its refusal before returning, so the client can
// exit while the outcome is still in flight.
func serveResolved(t *testing.T, resolve rsyncd.ModuleResolver) (port string, outcomes <-chan error) {
	t.Helper()

	return serveResolvedWithAccept(t, nil, resolve)
}

// serveResolvedWithAccept additionally runs onAccept for each connection before
// the client names a module, which is where a caller gating connections at the
// listener records what it approved.
func serveResolvedWithAccept(t *testing.T, onAccept func(remoteAddr string), resolve rsyncd.ModuleResolver) (port string, outcomes <-chan error) {
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

	// Buffered past any test's connection count so a handler never blocks.
	served := make(chan error, 8)
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			remoteAddr := conn.RemoteAddr().String()
			if onAccept != nil {
				onAccept(remoteAddr)
			}
			go func() {
				defer conn.Close()
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
// is relative and the port a flag because Cygwin rsync on Windows CI reads the
// colon in "C:\..." as a host separator.
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

	port, outcomes := serveResolved(t, reg.resolve)

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

	// A nil outcome is how a caller learns a transfer finished, so it has to be
	// asserted and not just inferred from the client exiting 0.
	for range migrations {
		if err := awaitOutcome(t, outcomes); err != nil {
			t.Errorf("completed transfer reported an error: %v", err)
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

			// A caller must be able to tell a refusal from a completed transfer.
			if err := awaitOutcome(t, outcomes); err == nil {
				t.Error("connection reported success despite being refused")
			}
		})
	}
}

// TestModuleResolverRetryAfterRejection covers a resolver refusing while the
// destination is not ready yet. The refusal has to leave the server able to
// serve the same module later, so a caller can treat it as "retry" rather than
// as a permanent failure.
func TestModuleResolverRetryAfterRejection(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "vol")
	writeTree(t, src, "payload")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	var ready atomic.Bool
	resolve := func(_, requestedModule string) (rsyncd.Module, error) {
		if !ready.Load() {
			return rsyncd.Module{}, errors.New("destination not ready")
		}
		return rsyncd.Module{Name: requestedModule, Path: dst, Writable: true}, nil
	}
	port, outcomes := serveResolvedWithAccept(t, nil, resolve)

	if err := push(t, rsyncBin, port, src, "migration-aaa/"); err == nil {
		t.Fatal("expected the first attempt to be refused")
	}
	if err := awaitOutcome(t, outcomes); err == nil {
		t.Error("refusal was not reported")
	}

	ready.Store(true)

	if err := push(t, rsyncBin, port, src, "migration-aaa/"); err != nil {
		t.Fatalf("retry after the destination became ready: %v", err)
	}
	if err := awaitOutcome(t, outcomes); err != nil {
		t.Errorf("retry reported an error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "data", "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("marker = %q, want %q", got, "payload")
	}
}

// TestModuleResolverServesOnlyApprovedConnections covers deciding at the
// listener what a connection may have and looking that up by address when it
// names a module, so the resolver cannot disagree with the gate that admitted it.
func TestModuleResolverServesOnlyApprovedConnections(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "vol")
	writeTree(t, src, "payload")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	const approvedModule = "migration-approved"

	var mu sync.Mutex
	approved := map[string]string{}
	onAccept := func(remoteAddr string) {
		mu.Lock()
		defer mu.Unlock()
		approved[remoteAddr] = approvedModule
	}

	resolve := func(remoteAddr, requestedModule string) (rsyncd.Module, error) {
		mu.Lock()
		want, ok := approved[remoteAddr]
		mu.Unlock()
		if !ok {
			return rsyncd.Module{}, fmt.Errorf("connection %s was not approved", remoteAddr)
		}
		if requestedModule != want {
			return rsyncd.Module{}, fmt.Errorf("connection %s is approved for %s, not %q", remoteAddr, want, requestedModule)
		}
		return rsyncd.Module{Name: requestedModule, Path: dst, Writable: true}, nil
	}
	port, outcomes := serveResolvedWithAccept(t, onAccept, resolve)

	if err := push(t, rsyncBin, port, src, approvedModule+"/"); err != nil {
		t.Fatalf("approved module: %v", err)
	}
	if err := awaitOutcome(t, outcomes); err != nil {
		t.Errorf("approved transfer reported an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "data", "marker")); err != nil {
		t.Fatalf("approved transfer did not land: %v", err)
	}

	// Same gate, different name: admitted at the listener but refused at resolve.
	if err := push(t, rsyncBin, port, src, "migration-other/"); err == nil {
		t.Fatal("expected a module the connection was not approved for to be refused")
	}
	if err := awaitOutcome(t, outcomes); err == nil {
		t.Error("refusal was not reported")
	}
	if _, err := os.Stat(filepath.Join(dst, "migration-other")); !os.IsNotExist(err) {
		t.Errorf("refused connection wrote into the approved root: %v", err)
	}
}

// TestModuleResolverRejectsRenamedModule covers a resolver serving a name other
// than the one requested, which would write a directory too deep rather than fail.
func TestModuleResolverRejectsRenamedModule(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "vol")
	writeTree(t, src, "data")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	// Right directory, wrong name: only the name differs.
	renaming := func(_, _ string) (rsyncd.Module, error) {
		return rsyncd.Module{Name: "canonical-name", Path: dst, Writable: true}, nil
	}
	port, outcomes := serveResolved(t, renaming)

	if err := push(t, rsyncBin, port, src, "requested-name/"); err == nil {
		t.Fatal("expected the client to fail")
	}
	if err := awaitOutcome(t, outcomes); err == nil {
		t.Error("connection reported success despite serving a renamed module")
	}

	for _, path := range []string{
		filepath.Join(dst, "data", "marker"),
		filepath.Join(dst, "requested-name", "data", "marker"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("rejected connection wrote %s: %v", path, statErr)
		}
	}
}
