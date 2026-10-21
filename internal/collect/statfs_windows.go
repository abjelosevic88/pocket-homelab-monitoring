package collect

import (
	"context"
	"os"
)

// The local collector reads Linux's /proc and /sys; on Windows the app can
// still watch other machines through agents and Glances.

type fsStat struct{ Bsize, Blocks, Bfree, Bavail, Files, Ffree uint64 }

func statfs(context.Context, string, bool) (fsStat, bool) { return fsStat{}, false }

func fileUID(os.FileInfo) (uint32, bool) { return 0, false }
