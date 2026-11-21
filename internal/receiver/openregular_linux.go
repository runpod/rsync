//go:build linux

package receiver

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openRegular opens name beneath root for reading, but only if it is a regular
// file; for anything else it returns a nil file and what is there.
//
// Whoever owns the destination tree can change it while a transfer runs, so a
// name the generator saw as a regular file may be a FIFO or a device node by
// the time it is opened. Opening a FIFO blocks until it gets a writer, and
// opening a device runs its open handler: a watchdog is armed, a tty can become
// the controlling terminal. O_PATH resolves the name without opening what it
// names, the type is checked on that handle, and the read handle is reopened
// from it through /proc, which reaches the inode already checked and not
// whatever the name points at by then.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	p, err := root.OpenFile(name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	defer p.Close()
	st, err := p.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, st, nil
	}
	f, err := os.OpenFile("/proc/self/fd/"+strconv.Itoa(int(p.Fd())), os.O_RDONLY, 0)
	if err != nil {
		// %v, not %w: without /proc this is ENOENT, which must not read to a
		// caller as the name itself being absent.
		return nil, nil, fmt.Errorf("reopen %s through /proc: %v", name, err)
	}
	return f, st, nil
}
