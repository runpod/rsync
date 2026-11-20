package receiver

import (
	"io/fs"
	"os"
)

// openRegular opens name beneath root for reading, but only if it is a regular
// file; for anything else it returns a nil file and what is there.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	f, err := root.Open(name)
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
