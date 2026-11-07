// Command s1-epoch-swap is the corrected version of the student PoC: v1 and
// v2 are compiled into the binary, the active one is published through
// internal/hotswap (per-version reference counting), state is handed over
// explicitly, and draining is event-driven rather than sleep-based.
package main

import (
	"flag"
	"log"
	"sync/atomic"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	drain := flag.Duration("drain-timeout", 30*time.Second, "max time to wait for the old version to drain")
	flag.Parse()

	counter := new(atomic.Uint64) // service state, owned by whichever version is current
	initial := &v1{counterImpl{version: "v1", count: counter}}

	s := svc.New(svc.Config{
		InitialName: "v1",
		Initial:     initial,
		Load:        build,
		Next: func(cur string) string {
			if cur == "v1" {
				return "v2"
			}
			return "v1"
		},
		Count:        counter.Load,
		DrainTimeout: *drain,
	})
	if err := s.Run(*addr); err != nil {
		log.Fatal(err)
	}
}
