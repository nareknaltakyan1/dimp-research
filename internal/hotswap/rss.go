package hotswap

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// MemSample is a coarse memory snapshot used by the memory-growth experiments.
type MemSample struct {
	RSSBytes     int64  // current resident set size (Linux /proc), or peak RSS elsewhere
	RSSIsPeak    bool   // true when only the peak (getrusage maxrss) is available
	HeapInuse    uint64 // Go heap in use (after GC)
	HeapSys      uint64
	NumGoroutine int
}

// SampleMemory forces a GC and returns process memory statistics.
func SampleMemory() MemSample {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := MemSample{HeapInuse: ms.HeapInuse, HeapSys: ms.HeapSys, NumGoroutine: runtime.NumGoroutine()}
	if rss, ok := procRSS(); ok {
		s.RSSBytes = rss
		return s
	}
	if rss, ok := psRSS(); ok {
		s.RSSBytes = rss
		return s
	}
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		s.RSSBytes = int64(ru.Maxrss) // bytes on darwin
		if runtime.GOOS == "linux" {
			s.RSSBytes *= 1024
		}
		s.RSSIsPeak = true
	}
	return s
}

func procRSS() (int64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					return kb * 1024, true
				}
			}
		}
	}
	return 0, false
}

// psRSS asks ps(1) for the current RSS (used on macOS, which has no /proc).
func psRSS() (int64, bool) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, false
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return kb * 1024, true
}
