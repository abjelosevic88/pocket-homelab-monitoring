//go:build !windows

package collect

import (
	"context"
	"os"
	"syscall"
	"time"
)

type fsStat struct{ Bsize, Blocks, Bfree, Bavail, Files, Ffree uint64 }

// statfs gives up after two seconds so a hung NFS server cannot stall a reading.
func statfs(ctx context.Context, path string, network bool) (fsStat, bool) {
	read := func() (fsStat, bool) {
		var st syscall.Statfs_t
		if syscall.Statfs(path, &st) != nil {
			return fsStat{}, false
		}
		return fsStat{uint64(st.Bsize), uint64(st.Blocks), uint64(st.Bfree), uint64(st.Bavail), uint64(st.Files), uint64(st.Ffree)}, true
	}
	if !network {
		return read()
	}
	type res struct {
		st fsStat
		ok bool
	}
	done := make(chan res, 1)
	go func() {
		st, ok := read()
		done <- res{st, ok}
	}()
	select {
	case r := <-done:
		return r.st, r.ok
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	return fsStat{}, false
}

func fileUID(fi os.FileInfo) (uint32, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid, true
	}
	return 0, false
}
