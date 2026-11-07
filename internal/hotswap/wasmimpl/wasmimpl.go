// Package wasmimpl hosts a business-logic implementation compiled to
// WebAssembly (WASI reactor) using wazero, and adapts it to svc.Impl.
//
// Each Module owns its own wazero.Runtime, the compiled code and a bounded
// pool of instances (a WASM instance is single-threaded, so concurrency comes
// from multiple instances). Close releases all of it, which is what makes
// WASM modules unloadable, unlike Go plugins.
package wasmimpl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Options configure a Module.
type Options struct {
	// PoolSize is the maximum number of concurrently used instances
	// (default 8). Instances are created lazily.
	PoolSize int
	// Prewarm instances created eagerly at load time (default 1, which
	// doubles as a smoke test of the module).
	Prewarm int
	// MemoryLimitPages caps each instance's linear memory (64 KiB pages;
	// default 512 = 32 MiB).
	MemoryLimitPages uint32
	// Interpreter forces the wazero interpreter instead of the compiler.
	Interpreter bool
}

type instance struct {
	mod    api.Module
	handle api.Function
	stack  []uint64
}

// Module is one loaded WASM version.
type Module struct {
	Name    string
	Version string // reported by the guest's version() export

	rt       wazero.Runtime
	compiled wazero.CompiledModule
	modCfg   wazero.ModuleConfig

	pool    chan *instance
	slots   chan struct{} // capacity = PoolSize; a token per existing instance
	mu      sync.Mutex    // guards closed
	closed  bool
	done    chan struct{} // closed by Close; unblocks waiters in get
	count   atomic.Pointer[atomic.Uint64]
	pid     uint32
	Created atomic.Int64 // number of instances ever created
}

// Load compiles wasm and prepares an instance pool.
func Load(ctx context.Context, name string, wasm []byte, o Options) (*Module, error) {
	if o.PoolSize <= 0 {
		o.PoolSize = 8
	}
	if o.Prewarm <= 0 {
		o.Prewarm = 1
	}
	if o.Prewarm > o.PoolSize {
		o.Prewarm = o.PoolSize
	}
	if o.MemoryLimitPages == 0 {
		o.MemoryLimitPages = 512
	}
	rc := wazero.NewRuntimeConfig()
	if o.Interpreter {
		rc = wazero.NewRuntimeConfigInterpreter()
	}
	rc = rc.WithMemoryLimitPages(o.MemoryLimitPages).WithCloseOnContextDone(false)
	rt := wazero.NewRuntimeWithConfig(ctx, rc)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("compile: %w", err)
	}
	if _, ok := compiled.ExportedFunctions()["handle"]; !ok {
		rt.Close(ctx)
		return nil, errors.New("module does not export handle")
	}
	m := &Module{
		Name:     name,
		rt:       rt,
		compiled: compiled,
		// Reactor: run _initialize (Go runtime init) instead of _start.
		// Name "" makes instances anonymous so several can coexist.
		modCfg: wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize").
			WithSysWalltime().WithSysNanotime().WithStderr(os.Stderr),
		pool:  make(chan *instance, o.PoolSize),
		slots: make(chan struct{}, o.PoolSize),
		done:  make(chan struct{}),
		pid:   uint32(os.Getpid()),
	}
	for i := 0; i < o.Prewarm; i++ {
		m.slots <- struct{}{}
		inst, err := m.newInstance(ctx)
		if err != nil {
			m.Close()
			return nil, err
		}
		if i == 0 {
			v, err := m.readVersion(ctx, inst)
			if err != nil {
				m.Close()
				return nil, err
			}
			m.Version = v
		}
		m.pool <- inst
	}
	return m, nil
}

func (m *Module) newInstance(ctx context.Context) (*instance, error) {
	mod, err := m.rt.InstantiateModule(ctx, m.compiled, m.modCfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	m.Created.Add(1)
	return &instance{mod: mod, handle: mod.ExportedFunction("handle"), stack: make([]uint64, 3)}, nil
}

func (m *Module) readVersion(ctx context.Context, inst *instance) (string, error) {
	res, err := inst.mod.ExportedFunction("version").Call(ctx)
	if err != nil {
		return "", err
	}
	b, ok := inst.mod.Memory().Read(uint32(res[0]>>32), uint32(res[0]))
	if !ok {
		return "", errors.New("version: out of bounds")
	}
	return string(b), nil
}

// get returns an idle instance, creating one if the pool is not full,
// otherwise waits for one to be returned.
func (m *Module) get(ctx context.Context) (*instance, error) {
	select {
	case <-m.done:
		return nil, ErrClosed
	default:
	}
	select {
	case inst := <-m.pool:
		return inst, nil
	default:
	}
	select {
	case inst := <-m.pool:
		return inst, nil
	case m.slots <- struct{}{}:
		inst, err := m.newInstance(ctx)
		if err != nil {
			<-m.slots
			return nil, err
		}
		return inst, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done:
		return nil, ErrClosed
	}
}

// ErrClosed is returned when a request reaches a module that was unloaded.
// With hotswap draining this cannot happen for requests that pinned the
// version; it guards against misuse.
var ErrClosed = errors.New("wasm module closed")

func (m *Module) discard(inst *instance) {
	inst.mod.Close(context.Background())
	<-m.slots
}

// ServeHTTP increments the host-owned counter and asks the guest to render
// the response body.
func (m *Module) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := m.count.Load()
	if c == nil {
		http.Error(w, "no state", http.StatusInternalServerError)
		return
	}
	inst, err := m.get(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	n := c.Add(1)
	inst.stack[0], inst.stack[1], inst.stack[2] = n, uint64(m.pid), uint64(time.Now().UnixNano())
	if err := inst.handle.CallWithStack(r.Context(), inst.stack); err != nil {
		m.discard(inst) // a trapped instance may be corrupt: never reuse it
		http.Error(w, "guest trap: "+err.Error(), http.StatusInternalServerError)
		return
	}
	packed := inst.stack[0]
	body, ok := inst.mod.Memory().Read(uint32(packed>>32), uint32(packed))
	if !ok {
		m.discard(inst)
		http.Error(w, "guest returned out-of-bounds buffer", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Version", m.Version)
	w.Header().Set("Content-Type", "application/json")
	w.Write(body) // copies out of linear memory before the instance is reused
	m.pool <- inst
}

// ExportState hands the counter handle to the successor.
func (m *Module) ExportState() (any, error) {
	c := m.count.Load()
	if c == nil {
		return nil, errors.New("no state")
	}
	return c, nil
}

// ImportState adopts the predecessor's counter.
func (m *Module) ImportState(s any) error {
	c, ok := s.(*atomic.Uint64)
	if !ok || c == nil {
		return fmt.Errorf("unexpected state %T", s)
	}
	m.count.Store(c)
	return nil
}

// Close unloads the module: closes all instances, the compiled code and the
// runtime. It must only be called once no request can use the module (after
// the hotswap version drained).
func (m *Module) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.done)
	m.mu.Unlock()
	ctx := context.Background()
	for {
		select {
		case inst := <-m.pool:
			inst.mod.Close(ctx)
			continue
		default:
		}
		break
	}
	return m.rt.Close(ctx) // closes compiled modules and frees code mappings
}
