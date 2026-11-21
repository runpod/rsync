//go:build !linux

package receiver

import (
	"io/fs"
	"os"
)

// openRegularWith opens name beneath root with flag, but only if it is a
// regular file; for anything else it returns a nil file and what is there.
//
// The type is checked before the open, so a FIFO or device node sitting at the
// name is never opened, and again on the handle. Without Linux's O_PATH that
// leaves a window between the two: something swapped in after the check is
// opened before it is refused.
func openRegularWith(root *os.Root, name string, flag int) (*os.File, fs.FileInfo, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, st, nil
	}
	f, err := root.OpenFile(name, flag, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err = f.Stat()
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
