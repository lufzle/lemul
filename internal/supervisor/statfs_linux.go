//go:build linux

package supervisor

import "golang.org/x/sys/unix"

// statfsUnit is the size of the blocks f_blocks counts.
//
// POSIX says that unit is f_frsize, and the distinction is not academic: on
// virtiofs f_bsize is 1 MiB against a 4 KiB fragment, so multiplying by f_bsize
// reported a 461 GB workspace as 115 TB. Wrong by exactly 256, and plausible
// enough to be waved away as the VM overcommitting -- which is what happened the
// first time it was read.
func statfsUnit(st *unix.Statfs_t) int64 {
	if st.Frsize > 0 {
		return int64(st.Frsize)
	}
	return int64(st.Bsize)
}
