package rsyncd_test

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/runpod/rsync/rsyncd"
)

func ExampleNewServer() {
	listener, err := net.Listen("tcp", "localhost:8873")
	if err != nil {
		log.Fatal(err)
	}

	rsyncServer, err := rsyncd.NewServer([]rsyncd.Module{
		{
			Name: "music",
			Path: "/home/bob/Music",
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	if err := rsyncServer.Serve(context.Background(), listener); err != nil {
		log.Fatal(err)
	}
}

// ExampleWithModuleResolver shows how to decide what to serve when a client
// connects, rather than listing modules up front. The resolver is given the
// connection's name and the module the client asked for, and returns the module
// to serve or an error to reject the connection.
func ExampleWithModuleResolver() {
	// Looked up live in a real server; a map keeps the example short.
	roots := map[string]string{
		"music":  "/home/bob/Music",
		"photos": "/home/bob/Photos",
	}

	resolve := func(_ context.Context, remoteAddr, requestedModule string) (rsyncd.Module, error) {
		path, ok := roots[requestedModule]
		if !ok {
			return rsyncd.Module{}, fmt.Errorf("unknown module %q", requestedModule)
		}
		return rsyncd.Module{
			Name: requestedModule, // must match what the client asked for
			Path: path,
		}, nil
	}

	listener, err := net.Listen("tcp", "localhost:8873")
	if err != nil {
		log.Fatal(err)
	}

	// The module list must be nil: the resolver answers for every module.
	// DontRestrict is required because the paths are not known yet.
	rsyncServer, err := rsyncd.NewServer(nil,
		rsyncd.WithModuleResolver(resolve),
		rsyncd.DontRestrict(),
	)
	if err != nil {
		log.Fatal(err)
	}

	if err := rsyncServer.Serve(context.Background(), listener); err != nil {
		log.Fatal(err)
	}
}

func TestNewServerModuleResolverRequiresDontRestrict(t *testing.T) {
	t.Parallel()

	resolve := func(context.Context, string, string) (rsyncd.Module, error) { return rsyncd.Module{}, nil }

	if _, err := rsyncd.NewServer(nil, rsyncd.WithModuleResolver(resolve)); err == nil {
		t.Fatal("NewServer accepted a resolver without DontRestrict")
	} else if !strings.Contains(err.Error(), "DontRestrict") {
		t.Errorf("error does not name the missing option: %v", err)
	}

	if _, err := rsyncd.NewServer(nil, rsyncd.WithModuleResolver(resolve), rsyncd.DontRestrict()); err != nil {
		t.Errorf("NewServer rejected a resolver with DontRestrict: %v", err)
	}
}

// TestNewServerModuleResolverRejectsStaticModules covers the combination being
// refused rather than silently ignored: getModule never consults the list once
// a resolver is set.
func TestNewServerModuleResolverRejectsStaticModules(t *testing.T) {
	t.Parallel()

	resolve := func(context.Context, string, string) (rsyncd.Module, error) { return rsyncd.Module{}, nil }
	modules := []rsyncd.Module{{Name: "music", Path: "/home/bob/Music"}}

	_, err := rsyncd.NewServer(modules, rsyncd.WithModuleResolver(resolve), rsyncd.DontRestrict())
	if err == nil {
		t.Fatal("NewServer accepted both a static module list and a resolver")
	}
	if !strings.Contains(err.Error(), "static module list") {
		t.Errorf("error does not explain the conflict: %v", err)
	}
}

// ExampleNewServer_fsFS shows how to serve an rsync module backed by an [fs.FS]
// instead of a filesystem path. This allows serving files from in-memory
// filesystems, embedded files, or any other [fs.FS] implementation.
func ExampleNewServer_fsFS() {
	// Create an in-memory filesystem using testing/fstest.
	// Any fs.FS implementation works (embed.FS, zip archives, etc.)
	memfs := fstest.MapFS{
		"hello.txt": &fstest.MapFile{
			Data: []byte("Hello from fs.FS!"),
			Mode: 0o644,
		},
		"readme.md": &fstest.MapFile{
			Data: []byte("# Welcome\nThis is served from memory."),
			Mode: 0o644,
		},
	}

	listener, err := net.Listen("tcp", "localhost:8873")
	if err != nil {
		log.Fatal(err)
	}

	rsyncServer, err := rsyncd.NewServer([]rsyncd.Module{
		{
			Name: "inmemory",
			FS:   memfs,
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("listening; now run: rsync -av rsync://%s/inmemory", listener.Addr())

	if err := rsyncServer.Serve(context.Background(), listener); err != nil {
		log.Fatal(err)
	}
}
