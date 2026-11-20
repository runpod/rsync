//go:build !linux

package receiver

import (
	"io/fs"
	"os"
)

// regularOrClose returns f and its info if f is a regular file. Otherwise it
// closes f and returns a nil file, with what f was.
func regularOrClose(f *os.File) (*os.File, fs.FileInfo, error) {
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
