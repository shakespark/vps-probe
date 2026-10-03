// Package atomicfile replaces files so that a reader, or a crash, finds
// either the old content or the new, never a mix: the content is written to
// a temporary file beside the target, synced and renamed over it.
package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
)

// Write puts data at path with mode perm.
func Write(path string, data []byte, perm os.FileMode) error {
	return write(path, data, func(f *os.File) error { return f.Chmod(perm) })
}

// Replace puts data at path, which must exist, keeping its mode and owner.
func Replace(path string, data []byte) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return write(path, data, func(f *os.File) error {
		if err := f.Chmod(st.Mode().Perm()); err != nil {
			return err
		}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			return f.Chown(int(sys.Uid), int(sys.Gid))
		}
		return nil
	})
}

func write(path string, data []byte, prepare func(*os.File) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*") // mode 0600 until prepare
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename

	if _, err = f.Write(data); err == nil {
		err = prepare(f)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
