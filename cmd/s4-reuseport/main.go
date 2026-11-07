// Command s4-reuseport is a contract server (docs/CONTRACT.md) that upgrades by
// handing off to a new process; see docs/solutions/ for the design.
package main

import "github.com/nareknaltakyan1/dimp-research/internal/handoff"

// Set at link time: -ldflags "-X main.version=v2 -X main.simulateFail=hang".
var (
	version      = "v1"
	simulateFail = ""
)

func main() { handoff.Main(handoff.ModeReusePort, version, simulateFail) }
