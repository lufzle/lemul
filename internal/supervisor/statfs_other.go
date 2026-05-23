//go:build !linux

package supervisor

import "golang.org/x/sys/unix"

// statfsUnit on everything that is not Linux.
//
// darwin's struct statfs has no f_frsize at all -- f_bsize IS the unit f_blocks
// is counted in there, so the Linux correction has nothing to correct. This file
// exists because the local driver runs on a developer's Mac, where the figure
// describes the dev machine rather than a sandbox and is only ever a sanity
// check.
func statfsUnit(st *unix.Statfs_t) int64 { return int64(st.Bsize) }
