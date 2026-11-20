package receiver

import (
	"io/fs"
	"os"
)

// partialSuffix is the sidecar a --partial transfer retains on interrupt.
const partialSuffix = ".partial"

// openPartialBasis opens "<name>.partial" as the delta basis for f. ok is false
// when no usable (regular, non-empty) partial exists.
func (rt *Transfer) openPartialBasis(f *File) (in *os.File, size int64, ok bool) {
	in, st, err := openRegular(rt.DestRoot, f.Name+partialSuffix)
	if err != nil || in == nil {
		return nil, 0, false
	}
	if st.Size() == 0 {
		in.Close()
		return nil, 0, false
	}
	return in, st.Size(), true
}

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

// retainPartial promotes the temp to "<name>.partial", keeping whichever is
// longer so the basis only advances. Names are relative to the destination root.
func (rt *Transfer) retainPartial(tmpName, partialName string) {
	tmpSize := rt.rootFileSize(tmpName)
	if tmpSize < 0 {
		return // nothing written this attempt
	}
	if tmpSize < rt.rootFileSize(partialName) {
		_ = rt.DestRoot.Remove(tmpName) // existing partial is more complete
		return
	}
	if err := rt.DestRoot.Rename(tmpName, partialName); err != nil {
		rt.Logger.Printf("failed to retain partial %s: %v", partialName, err)
		_ = rt.DestRoot.Remove(tmpName)
	}
}

func (rt *Transfer) rootFileSize(name string) int64 {
	st, err := rt.DestRoot.Stat(name)
	if err != nil {
		return -1
	}
	return st.Size()
}
