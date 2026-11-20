//go:build !windows

package receiver

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
)

// symlink creates newname as a symlink to oldname, atomically replacing a
// symlink that is already there.
//
// Everything goes through root. renameio.SymlinkRoot does not: it makes its
// temporary directory with os.MkdirTemp(root.Name()), by path, so once the
// directory the root was opened on is swapped for a symlink, the temporary
// lands wherever that points.
func symlink(root *os.Root, oldname, newname string) error {
	// Fast path: nothing to replace.
	if err := root.Symlink(oldname, newname); err == nil || !os.IsExist(err) {
		return err
	}

	// A fixed-length name beside newname: the same directory keeps the rename
	// atomic, and not deriving it from newname keeps it within NAME_MAX.
	dir := filepath.Dir(newname)
	for attempt := 0; ; attempt++ {
		tmp := filepath.Join(dir, ".rsync-symlink-"+strconv.FormatInt(rand.Int64(), 10))
		err := root.Symlink(oldname, tmp)
		if os.IsExist(err) && attempt < 10000 {
			continue
		}
		if err != nil {
			return err
		}
		// A crash between the two calls leaves tmp behind.
		if err := root.Rename(tmp, newname); err != nil {
			root.Remove(tmp)
			return err
		}
		return nil
	}
}
