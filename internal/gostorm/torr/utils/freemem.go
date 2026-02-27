package utils

import (
	"runtime"
	"runtime/debug"
)

func FreeOSMemGC() {
	runtime.GC()
	debug.FreeOSMemory()
}
