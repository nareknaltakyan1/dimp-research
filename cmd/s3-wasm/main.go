// Command s3-wasm hot-loads business logic compiled to WebAssembly (WASI
// reactor modules, run by wazero - pure Go, no cgo). Upgrades compile the new
// module off the request path, publish it through internal/hotswap and, once
// the old module has drained, close it so its memory is reclaimed.
//
//	./cmd/s3-wasm/build.sh            # bin/s3-wasm + bin/modules/{v1,v2}.wasm
//	./cmd/s3-wasm/bin/s3-wasm -addr :8080
//	curl -XPOST 'localhost:8080/admin/upgrade?to=v2'
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/wasmimpl"
)

func defaultModuleDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "modules")
	}
	return "modules"
}

func loadModule(dir, name string, o wasmimpl.Options) (*wasmimpl.Module, error) {
	b, err := os.ReadFile(filepath.Join(dir, name+".wasm"))
	if err != nil {
		return nil, err
	}
	return wasmimpl.Load(context.Background(), name, b, o)
}

func newServer(dir, initial string, o wasmimpl.Options, drain time.Duration, events *svc.EventLog) (*svc.Server, error) {
	counter := new(atomic.Uint64) // host-owned service state
	first, err := loadModule(dir, initial, o)
	if err != nil {
		return nil, err
	}
	if err := first.ImportState(counter); err != nil {
		return nil, err
	}
	s := svc.New(svc.Config{
		InitialName: initial,
		Initial:     first,
		Load:        func(name string) (svc.Impl, error) { return loadModule(dir, name, o) },
		Next: func(cur string) string {
			if cur == "v1" {
				return "v2"
			}
			return "v1"
		},
		Count:        counter.Load,
		DrainTimeout: drain,
		Events:       events,
		Unload: func(name string, impl svc.Impl) {
			if m, ok := impl.(*wasmimpl.Module); ok {
				m.Close()
			}
		},
	})
	return s, nil
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dir := flag.String("module-dir", defaultModuleDir(), "directory containing <version>.wasm files")
	initial := flag.String("initial", "v1", "initial module name")
	pool := flag.Int("pool", 8, "max WASM instances per module (concurrency)")
	interp := flag.Bool("interpreter", false, "use the wazero interpreter instead of the compiler")
	drain := flag.Duration("drain-timeout", 30*time.Second, "max time to wait for the old version to drain")
	flag.Parse()
	s, err := newServer(*dir, *initial, wasmimpl.Options{PoolSize: *pool, Interpreter: *interp}, *drain, nil)
	if err != nil {
		log.Fatalf("load initial module: %v", err)
	}
	if err := s.Run(*addr); err != nil {
		log.Fatal(err)
	}
}
