//go:build !linux && !windows

package receiver

import (
	"io/fs"
	"os"
	"syscall"
)

// openRegular opens name beneath root for reading, but only if it is a regular
// file; for anything else it returns a nil file and what is there. See the
// Linux version for why. Without O_PATH the open itself cannot be avoided, so
// O_NONBLOCK keeps a FIFO from blocking it and the type is checked on the
// opened handle.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, st, nil
	}
	return f, st, nil
}
