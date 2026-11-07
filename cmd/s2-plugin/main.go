// Command s2-plugin performs true dynamic code loading with Go's plugin
// package: business logic lives in separately built .so files that are
// loaded at upgrade time and published through internal/hotswap.
//
//	./cmd/s2-plugin/build.sh          # builds bin/s2-plugin + bin/plugins/{v1,v2}.so
//	./cmd/s2-plugin/bin/s2-plugin -addr :8080
//	curl -XPOST 'localhost:8080/admin/upgrade?to=v2'   # loads plugins/v2.so
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/pluginimpl"
	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
)

func defaultPluginDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "plugins")
	}
	return "plugins"
}

// newServer loads the initial plugin and wires the contract server.
func newServer(dir, initial string, drain time.Duration, events *svc.EventLog) (*svc.Server, *atomic.Uint64, error) {
	counter := new(atomic.Uint64) // host-owned state cell, handed between plugins
	load := func(name string) (svc.Impl, error) {
		return pluginimpl.Open(filepath.Join(dir, name+".so"))
	}
	first, err := pluginimpl.Open(filepath.Join(dir, initial+".so"))
	if err != nil {
		return nil, nil, err
	}
	if err := first.ImportState(counter); err != nil {
		return nil, nil, err
	}
	s := svc.New(svc.Config{
		InitialName: initial,
		Initial:     first,
		Load:        load,
		Next: func(cur string) string {
			if cur == "v1" {
				return "v2"
			}
			return "v1"
		},
		Count:        counter.Load,
		DrainTimeout: drain,
		Events:       events,
		// Unload: nil - Go plugins can never be unloaded (see docs/solutions/S2-go-plugin.md).
	})
	return s, counter, nil
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dir := flag.String("plugin-dir", defaultPluginDir(), "directory containing <version>.so files")
	initial := flag.String("initial", "v1", "initial plugin name")
	drain := flag.Duration("drain-timeout", 30*time.Second, "max time to wait for the old version to drain")
	flag.Parse()
	if !pluginimpl.Supported {
		log.Fatal("this binary was built without plugin support (needs cgo on linux/darwin)")
	}
	s, _, err := newServer(*dir, *initial, *drain, nil)
	if err != nil {
		log.Fatalf("load initial plugin: %v", err)
	}
	if err := s.Run(*addr); err != nil {
		log.Fatal(err)
	}
}
