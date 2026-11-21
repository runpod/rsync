//go:build !linux && !windows

package receiver

import (
	"io/fs"
	"os"
	"syscall"
)

// openRegular opens name beneath root for reading, but only if it is a regular
// file; for anything else it returns a nil file and what is there. See the
// Linux version for why, and openRegularWith for what this one cannot close.
// O_NONBLOCK keeps a FIFO swapped in after the check from blocking the open.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	return openRegularWith(root, name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW)
}
