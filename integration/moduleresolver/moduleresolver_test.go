// Package moduleresolver_test covers serving a module whose path is computed
// when a client connects, which lets a caller route each connection to a
// different directory without knowing the set of directories at startup.
package moduleresolver_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runpod/rsync/internal/rsynctest"
	"github.com/runpod/rsync/internal/testlogger"
	"github.com/runpod/rsync/rsyncd"
)

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
	directories := map[string]string{"mod-a": dest}
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

	// "mod-a" is the module name; it is what reaches the resolver.
	rsync := exec.Command(rsyncBin,
		"--archive",
		"--port="+port,
		filepath.Base(source)+"/",
		"rsync://localhost/mod-a/")
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

// rootRegistry maps a name supplied by the client to a directory.
type rootRegistry map[string]string

func (r rootRegistry) resolve(remoteAddr, requestedModule string) (rsyncd.Module, error) {
	dir, ok := r[requestedModule]
	if !ok {
		return rsyncd.Module{}, fmt.Errorf("unknown module %q", requestedModule)
	}
	return rsyncd.Module{Name: requestedModule, Path: dir, Writable: true}, nil
}

// serveResolved runs the daemon over its own accept loop rather than Serve, so
// each connection's outcome is observable. Outcomes must be awaited rather than
// sampled: the daemon writes its refusal before returning, so the client can
// exit while the outcome is still in flight.
func serveResolved(t *testing.T, resolve rsyncd.ModuleResolver) (port string, outcomes <-chan error) {
	t.Helper()

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return serveResolvedOn(t, ln, nil, resolve)
}

// serveResolvedOn is serveResolved over a caller-supplied listener. connName
// overrides the name given to NewConnection, which defaults to the peer address.
func serveResolvedOn(t *testing.T, ln net.Listener, connName func(net.Conn) string, resolve rsyncd.ModuleResolver) (port string, outcomes <-chan error) {
	t.Helper()

	srv, err := rsyncd.NewServer(nil,
		rsyncd.WithModuleResolver(resolve),
		rsyncd.WithStderr(testlogger.New(t)),
		rsyncd.DontRestrict(),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
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
			name := conn.RemoteAddr().String()
			if connName != nil {
				name = connName(conn)
			}
			go func() {
				defer conn.Close()
				served <- srv.HandleDaemonConn(ctx, rsyncd.NewConnection(conn, conn, name))
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

// TestModuleResolverRoutesConcurrentConnections is the core check: the resolver
// runs per connection, so two clients naming different modules are served
// different roots at the same time and neither tree leaks into the other.
func TestModuleResolverRoutesConcurrentConnections(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	reg := rootRegistry{}

	type pair struct{ name, src, dst string }
	pairs := []pair{
		{name: "mod-a", src: filepath.Join(tmp, "src-a"), dst: filepath.Join(tmp, "root-a")},
		{name: "mod-b", src: filepath.Join(tmp, "src-b"), dst: filepath.Join(tmp, "root-b")},
	}
	for _, p := range pairs {
		writeTree(t, p.src, p.name)
		if err := os.MkdirAll(p.dst, 0o755); err != nil {
			t.Fatal(err)
		}
		reg[p.name] = p.dst
	}

	port, outcomes := serveResolved(t, reg.resolve)

	var wg sync.WaitGroup
	errs := make([]error, len(pairs))
	for i, p := range pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = push(t, rsyncBin, port, p.src, p.name+"/")
		}()
	}
	wg.Wait()

	for i, p := range pairs {
		if errs[i] != nil {
			t.Fatalf("%s: push failed: %v", p.name, errs[i])
		}

		got, err := os.ReadFile(filepath.Join(p.dst, "data", "marker"))
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		if string(got) != p.name {
			t.Errorf("%s: landed in the wrong root: marker = %q", p.name, got)
		}
	}

	// HandleDaemonConn reports a completed transfer by returning nil.
	for range pairs {
		if err := awaitOutcome(t, outcomes); err != nil {
			t.Errorf("completed transfer reported an error: %v", err)
		}
	}
}

// TestModuleResolverConfinesToResolvedRoot checks the isolation property a
// shared root cannot offer: a client naming another root's directory stays
// inside the root it was given, because that root is its whole view.
func TestModuleResolverConfinesToResolvedRoot(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	mine := filepath.Join(tmp, "root-a")
	other := filepath.Join(tmp, "root-b")
	writeTree(t, src, "payload")
	for _, d := range []string{mine, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	reg := rootRegistry{"mod-a": mine, "mod-b": other}
	port, _ := serveResolved(t, reg.resolve)

	// Served mod-a, aiming at the other root's directory name.
	if err := push(t, rsyncBin, port, src, "mod-a/root-b/"); err != nil {
		t.Fatalf("push failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(other, "data", "marker")); !os.IsNotExist(err) {
		t.Fatalf("write reached the other root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mine, "root-b", "data", "marker")); err != nil {
		t.Fatalf("write should have stayed inside the resolved root: %v", err)
	}
}

// TestModuleResolverRejectsUnknownModule covers a resolver error reaching both
// ends: the client sees @ERROR and HandleDaemonConn returns the error, rather
// than it being swallowed.
func TestModuleResolverRejectsUnknownModule(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "root-a")
	writeTree(t, src, "payload")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := rootRegistry{"mod-a": dst}
	port, outcomes := serveResolved(t, reg.resolve)

	if err := push(t, rsyncBin, port, src, "mod-unknown/"); err == nil {
		t.Fatal("expected the client to fail")
	}
	if _, err := os.Stat(filepath.Join(dst, "data", "marker")); !os.IsNotExist(err) {
		t.Fatalf("rejected connection still wrote data: %v", err)
	}

	// A caller must be able to tell a refusal from a completed transfer.
	if err := awaitOutcome(t, outcomes); err == nil {
		t.Error("connection reported success despite being refused")
	}
}

// TestModuleResolverRejectsRenamedModule covers a resolver serving a name other
// than the one requested, which would write a directory too deep rather than fail.
func TestModuleResolverRejectsRenamedModule(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "root-a")
	writeTree(t, src, "payload")
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
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("rejected connection wrote %s: %v", path, err)
		}
	}
}

// TestModuleResolverRejectsInvalidModule covers a resolved module going through
// the same validation as a static one, so a resolver cannot serve something
// NewServer would have refused.
func TestModuleResolverRejectsInvalidModule(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	writeTree(t, src, "payload")

	// Empty Path: valid-looking, but validateModule rejects it.
	resolve := func(_, requestedModule string) (rsyncd.Module, error) {
		return rsyncd.Module{Name: requestedModule, Writable: true}, nil
	}
	port, outcomes := serveResolved(t, resolve)

	if err := push(t, rsyncBin, port, src, "mod-a/"); err == nil {
		t.Fatal("expected the client to fail")
	}
	// Assert on the validation error specifically: an empty path would fail
	// later anyway, so a bare "it failed" would pass without validateModule.
	err := awaitOutcome(t, outcomes)
	if err == nil {
		t.Fatal("connection reported success despite an invalid module")
	}
	if want := "resolved module:"; !strings.Contains(err.Error(), want) {
		t.Errorf("outcome %q does not report a validation failure (%q)", err, want)
	}
}

// TestModuleResolverEnforcesACL covers a resolved module's ACL being applied,
// which is how a caller restricts a module to particular sources without
// inspecting addresses in the resolver itself.
func TestModuleResolverEnforcesACL(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	for _, tt := range []struct {
		remoteAddr string
		wantError  bool
	}{
		{remoteAddr: "192.168.1.1", wantError: false},
		{remoteAddr: "10.0.0.1", wantError: true},
	} {
		t.Run(tt.remoteAddr, func(t *testing.T) {
			t.Parallel()

			tmp := t.TempDir()
			src := filepath.Join(tmp, "src")
			dst := filepath.Join(tmp, "root-a")
			writeTree(t, src, "payload")
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatal(err)
			}

			resolve := func(_, requestedModule string) (rsyncd.Module, error) {
				return rsyncd.Module{
					Name:     requestedModule,
					Path:     dst,
					Writable: true,
					ACL:      []string{"allow 192.168.1.0/24", "deny all"},
				}, nil
			}

			ln, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatal(err)
			}
			port, outcomes := serveResolvedOn(t, &connWithRemoteAddrListener{
				Listener:   ln,
				remoteAddr: &net.TCPAddr{IP: net.ParseIP(tt.remoteAddr), Port: 1234},
			}, nil, resolve)

			pushErr := push(t, rsyncBin, port, src, "mod-a/")
			outcome := awaitOutcome(t, outcomes)

			if tt.wantError {
				if pushErr == nil {
					t.Error("expected the client to be denied")
				}
				if outcome == nil {
					t.Error("denied connection reported success")
				}
				if _, err := os.Stat(filepath.Join(dst, "data", "marker")); !os.IsNotExist(err) {
					t.Errorf("denied connection still wrote data: %v", err)
				}
				return
			}

			if pushErr != nil {
				t.Errorf("allowed source was denied: %v", pushErr)
			}
			if outcome != nil {
				t.Errorf("allowed transfer reported an error: %v", outcome)
			}
			if _, err := os.Stat(filepath.Join(dst, "data", "marker")); err != nil {
				t.Errorf("allowed transfer did not land: %v", err)
			}
		})
	}
}

// TestModuleResolverReceivesConnectionName covers what the resolver's first
// argument actually carries. Serve passes the peer address, but a caller
// driving HandleDaemonConn supplies the name itself and may have no address to
// give, so a resolver must not assume one.
func TestModuleResolverReceivesConnectionName(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	t.Run("peer address", func(t *testing.T) {
		t.Parallel()

		tmp := t.TempDir()
		src := filepath.Join(tmp, "src")
		dst := filepath.Join(tmp, "root-a")
		writeTree(t, src, "payload")
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatal(err)
		}

		const want = "192.0.2.10:1234"
		var mu sync.Mutex
		var got string
		resolve := func(remoteAddr, requestedModule string) (rsyncd.Module, error) {
			mu.Lock()
			got = remoteAddr
			mu.Unlock()
			return rsyncd.Module{Name: requestedModule, Path: dst, Writable: true}, nil
		}

		ln, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatal(err)
		}
		port, outcomes := serveResolvedOn(t, &connWithRemoteAddrListener{
			Listener:   ln,
			remoteAddr: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 1234},
		}, nil, resolve)

		if err := push(t, rsyncBin, port, src, "mod-a/"); err != nil {
			t.Fatalf("push failed: %v", err)
		}
		if err := awaitOutcome(t, outcomes); err != nil {
			t.Errorf("transfer reported an error: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		if got != want {
			t.Errorf("resolver saw remoteAddr %q, want %q", got, want)
		}
	})

	t.Run("placeholder name", func(t *testing.T) {
		t.Parallel()

		tmp := t.TempDir()
		src := filepath.Join(tmp, "src")
		dst := filepath.Join(tmp, "root-a")
		writeTree(t, src, "payload")
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatal(err)
		}

		// What maincmd passes for a daemon spoken over a remote shell.
		const want = "<remote-shell-daemon>"
		var mu sync.Mutex
		var got string
		resolve := func(remoteAddr, requestedModule string) (rsyncd.Module, error) {
			mu.Lock()
			got = remoteAddr
			mu.Unlock()
			return rsyncd.Module{Name: requestedModule, Path: dst, Writable: true}, nil
		}

		ln, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatal(err)
		}
		port, outcomes := serveResolvedOn(t, ln, func(net.Conn) string { return want }, resolve)

		if err := push(t, rsyncBin, port, src, "mod-a/"); err != nil {
			t.Fatalf("push failed: %v", err)
		}
		if err := awaitOutcome(t, outcomes); err != nil {
			t.Errorf("transfer reported an error: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		if got != want {
			t.Errorf("resolver saw remoteAddr %q, want %q", got, want)
		}
	})
}

// TestModuleResolverListsNoModules pins the module listing being empty with a
// resolver: there is no set of modules to enumerate before a client names one.
func TestModuleResolverListsNoModules(t *testing.T) {
	t.Parallel()

	rsyncBin := rsynctest.TridgeOrGTFO(t, "test drives a real rsync client against the daemon")

	tmp := t.TempDir()
	dst := filepath.Join(tmp, "root-a")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := rootRegistry{"mod-a": dst}
	port, _ := serveResolved(t, reg.resolve)

	var stdout bytes.Buffer
	list := exec.Command(rsyncBin, "--port="+port, "rsync://localhost/")
	list.Env = append(os.Environ(), "LANG=C.UTF-8")
	list.Stdout = &stdout
	list.Stderr = testlogger.New(t)
	if err := list.Run(); err != nil {
		t.Fatalf("module listing: %v", err)
	}

	if got := stdout.String(); got != "" {
		t.Errorf("module listing = %q, want empty: a resolver has no list to enumerate", got)
	}
}

// connWithRemoteAddrListener reports a fixed peer address for every accepted
// connection, so address-dependent behaviour can be driven from localhost.
type connWithRemoteAddrListener struct {
	net.Listener

	remoteAddr net.Addr
}

func (l *connWithRemoteAddrListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &connWithRemoteAddr{Conn: conn, remoteAddr: l.remoteAddr}, nil
}

type connWithRemoteAddr struct {
	net.Conn

	remoteAddr net.Addr
}

func (c *connWithRemoteAddr) RemoteAddr() net.Addr { return c.remoteAddr }
