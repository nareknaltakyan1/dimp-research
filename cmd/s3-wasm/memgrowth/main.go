// Command memgrowth measures process memory over repeated WASM upgrades
// (v1 -> v2 -> v1 ...). Each upgrade compiles a fresh module in its own
// wazero runtime, swaps it in through internal/hotswap under concurrent load,
// waits for the old version to drain and then unloads it (Close), unless
// -no-unload is given, which emulates the plugin situation.
//
//	cmd/s3-wasm/build.sh && cmd/s3-wasm/bin/memgrowth -n 30
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
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/wasmimpl"
)

func main() {
	exe, _ := os.Executable()
	dir := flag.String("module-dir", filepath.Join(filepath.Dir(exe), "modules"), "module directory")
	n := flag.Int("n", 20, "number of upgrades")
	pool := flag.Int("pool", 4, "instances per module")
	noUnload := flag.Bool("no-unload", false, "never Close old modules and keep them referenced (emulates plugins, which cannot be unloaded)")
	interp := flag.Bool("interpreter", false, "use the wazero interpreter")
	flag.Parse()

	wasm := map[string][]byte{}
	for _, v := range []string{"v1", "v2"} {
		b, err := os.ReadFile(filepath.Join(*dir, v+".wasm"))
		if err != nil {
			log.Fatal(err)
		}
		wasm[v] = b
	}
	o := wasmimpl.Options{PoolSize: *pool, Prewarm: *pool, Interpreter: *interp}
	ctx := context.Background()
	counter := new(atomic.Uint64)
	first, err := wasmimpl.Load(ctx, "v1", wasm["v1"], o)
	if err != nil {
		log.Fatal(err)
	}
	first.ImportState(counter)
	reg := hotswap.New[svc.Impl]("v1", first)
	var retained []*wasmimpl.Module // only used with -no-unload

	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < *pool; w++ {
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
		name := "v2"
		if i%2 == 0 {
			name = "v1"
		}
		t0 := time.Now()
		m, err := wasmimpl.Load(ctx, name, wasm[name], o)
		if err != nil {
			log.Fatal(err)
		}
		loadDur := time.Since(t0)
		u, err := reg.Swap(name, m, hotswap.SwapOptions[svc.Impl]{})
		if err != nil {
			log.Fatal(err)
		}
		t1 := time.Now()
		if err := u.Wait(ctx, 10*time.Second); err != nil {
			log.Fatal(err)
		}
		drainDur := time.Since(t1)
		if *noUnload {
			retained = append(retained, u.Old.Impl.(*wasmimpl.Module))
		} else {
			u.Old.Impl.(*wasmimpl.Module).Close()
		}
		s := hotswap.SampleMemory()
		fmt.Printf("%d,%d,%d,%d,%.3f,%.3f,%s,%d\n", i, s.RSSBytes, s.RSSBytes-base.RSSBytes, s.HeapInuse,
			float64(loadDur.Microseconds())/1000, float64(drainDur.Microseconds())/1000, m.Version, counter.Load())
	}
	stop.Store(true)
	wg.Wait()
	fmt.Fprintf(os.Stderr, "retained modules: %d\n", len(retained))
}
