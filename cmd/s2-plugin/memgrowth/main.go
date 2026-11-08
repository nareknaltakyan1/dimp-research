// Command memgrowth measures process memory over repeated plugin upgrades.
// Each upgrade loads a distinct plugin generation (v2.N.so), swaps it in
// through internal/hotswap under light concurrent load and waits for the old
// version to drain. Because Go plugins can never be unloaded, every
// generation's code, data and type metadata stay mapped forever.
//
//	GENS=30 cmd/s2-plugin/build.sh /tmp/s2 && /tmp/s2/memgrowth -n 30
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/pluginimpl"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
)

func main() {
	exe, _ := os.Executable()
	dir := flag.String("plugin-dir", filepath.Join(filepath.Dir(exe), "plugins"), "plugin directory")
	n := flag.Int("n", 20, "number of upgrades (needs v2.1..N.so)")
	flag.Parse()

	counter := new(atomic.Uint64)
	first, err := pluginimpl.Open(filepath.Join(*dir, "v1.so"))
	if err != nil {
		log.Fatal(err)
	}
	if err := first.ImportState(counter); err != nil {
		log.Fatal(err)
	}
	reg := hotswap.New[svc.Impl]("v1", first)

	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				v := reg.Acquire()
				v.Impl.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api", nil))
				v.Release()
			}
		}()
	}

	base := hotswap.SampleMemory()
	fmt.Printf("gen,rss_bytes,rss_delta_bytes,heap_inuse_bytes,load_ms,drain_ms,version,count\n")
	fmt.Printf("0,%d,0,%d,0,0,v1,%d\n", base.RSSBytes, base.HeapInuse, counter.Load())
	for i := 1; i <= *n; i++ {
		t0 := time.Now()
		impl, err := pluginimpl.Open(filepath.Join(*dir, fmt.Sprintf("v2.%d.so", i)))
		if err != nil {
			log.Fatalf("gen %d: %v", i, err)
		}
		loadDur := time.Since(t0)
		u, err := reg.Swap(fmt.Sprintf("v2.%d", i), impl, hotswap.SwapOptions[svc.Impl]{})
		if err != nil {
			log.Fatal(err)
		}
		t1 := time.Now()
		if err := u.Wait(context.Background(), 10*time.Second); err != nil {
			log.Fatal(err)
		}
		drainDur := time.Since(t1)
		m := hotswap.SampleMemory()
		fmt.Printf("%d,%d,%d,%d,%.3f,%.3f,%s,%d\n", i, m.RSSBytes, m.RSSBytes-base.RSSBytes, m.HeapInuse,
			float64(loadDur.Microseconds())/1000, float64(drainDur.Microseconds())/1000, impl.Version, counter.Load())
	}
	stop.Store(true)
	wg.Wait()
}
