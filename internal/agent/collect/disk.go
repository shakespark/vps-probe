package collect

import (
	"path/filepath"
	"syscall"
)

// Disk is the usage of the filesystem at a mount point.
type Disk struct {
	Mount              string
	Total, Used, Avail uint64
	InodePct           float64
}

// Disk matches df: used = blocks - free, avail = blocks available to
// unprivileged users.
func (f FS) Disk(mount string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Join(f.Root, mount), &st); err != nil {
		return Disk{}, err
	}
	bs := uint64(st.Frsize)
	if bs == 0 {
		bs = uint64(st.Bsize)
	}
	d := Disk{
		Mount: mount,
		Total: st.Blocks * bs,
		Used:  sub(st.Blocks, st.Bfree) * bs,
		Avail: st.Bavail * bs,
	}
	if st.Files > 0 {
		d.InodePct = float64(sub(st.Files, st.Ffree)) / float64(st.Files) * 100
	}
	return d, nil
}
