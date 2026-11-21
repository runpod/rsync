package receiver

import (
	"io/fs"
	"os"
)

// openRegular opens name beneath root for reading, but only if it is a regular
// file; for anything else it returns a nil file and what is there.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	return openRegularWith(root, name, os.O_RDONLY)
}
