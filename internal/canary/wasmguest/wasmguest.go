// Package wasmguest hosts S7 business-logic modules (WASI reactors run by
// wazero). It follows internal/hotswap/wasmimpl (S3) but uses the S7 ABI,
// which adds a business input so that stable and candidate responses can be
// compared in shadow mode:
//
//	handle(count u64, pid u32, ts_ns i64, x i64) -> u64   packed ptr<<32|len of the JSON body
//	version() -> u64                                     packed ptr<<32|len of the version string
//
// The guest is a pure function of its arguments: service state (the
// counter) is owned by the host and passed in by value. Each Module owns its
// own wazero.Runtime; Close frees instances, compiled code and the runtime.
// Shadow calls use a separate instance pool so that, even for a guest that
// kept internal memory between calls, mirrored traffic could not leak into
// the instances that serve clients.
package wasmguest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Options configure a Module.
type Options struct {
	PoolSize int // max instances per pool (default 8)
	// Prewarm instances are created eagerly in EACH pool at load time
	// (default 2), off the request path. Lazy creation of a Go/WASI instance
	// costs milliseconds (Go runtime _initialize), which would otherwise be
	// charged to the first requests a candidate serves.
	Prewarm          int
	MemoryLimitPages uint32 // per-instance linear memory cap (default 512 = 32 MiB)
	Interpreter      bool
}

type instance struct {
	mod    api.Module
	handle api.Function
	stack  []uint64
}

type pool struct {
	m     *Module
	idle  chan *instance
	slots chan struct{}
}

// Module is one loaded version.
type Module struct {
	Name    string
	Version string // from the guest's version() export

	rt       wazero.Runtime
	compiled wazero.CompiledModule
	modCfg   wazero.ModuleConfig
	serve    *pool
	shadow   *pool

	mu      sync.Mutex
	closed  bool
	done    chan struct{}
	Created atomic.Int64
}

// ErrClosed is returned for calls on an unloaded module.
var ErrClosed = errors.New("wasmguest: module closed")

// Load compiles wasm, instantiates one instance (smoke test: reads version)
// and returns the module.
func Load(ctx context.Context, name string, wasm []byte, o Options) (*Module, error) {
	if o.PoolSize <= 0 {
		o.PoolSize = 8
	}
	if o.MemoryLimitPages == 0 {
		o.MemoryLimitPages = 512
	}
	if o.Prewarm <= 0 {
		o.Prewarm = 2
	}
	if o.Prewarm > o.PoolSize {
		o.Prewarm = o.PoolSize
	}
	rc := wazero.NewRuntimeConfig()
	if o.Interpreter {
		rc = wazero.NewRuntimeConfigInterpreter()
	}
	rc = rc.WithMemoryLimitPages(o.MemoryLimitPages).WithCloseOnContextDone(true)
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
	for _, fn := range []string{"handle", "version"} {
		if _, ok := compiled.ExportedFunctions()[fn]; !ok {
			rt.Close(ctx)
			return nil, fmt.Errorf("module does not export %s", fn)
		}
	}
	m := &Module{
		Name:     name,
		rt:       rt,
		compiled: compiled,
		// No stdout, no filesystem, no env, no args: the only capabilities a
		// guest has are clocks and stderr (for panics).
		modCfg: wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize").
			WithSysWalltime().WithSysNanotime(),
		done: make(chan struct{}),
	}
	m.serve = &pool{m: m, idle: make(chan *instance, o.PoolSize), slots: make(chan struct{}, o.PoolSize)}
	m.shadow = &pool{m: m, idle: make(chan *instance, o.PoolSize), slots: make(chan struct{}, o.PoolSize)}
	inst, err := m.serve.get(ctx)
	if err != nil {
		m.Close()
		return nil, err
	}
	v, err := m.readVersion(ctx, inst)
	if err != nil {
		m.Close()
		return nil, err
	}
	m.Version = v
	m.serve.idle <- inst
	for _, p := range []*pool{m.serve, m.shadow} {
		var got []*instance
		for len(p.idle)+len(got) < o.Prewarm {
			inst, err := p.get(ctx)
			if err != nil {
				m.Close()
				return nil, err
			}
			got = append(got, inst)
		}
		for _, inst := range got {
			p.idle <- inst
		}
	}
	return m, nil
}

func (m *Module) newInstance(ctx context.Context) (*instance, error) {
	mod, err := m.rt.InstantiateModule(ctx, m.compiled, m.modCfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	m.Created.Add(1)
	return &instance{mod: mod, handle: mod.ExportedFunction("handle"), stack: make([]uint64, 4)}, nil
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

func (p *pool) get(ctx context.Context) (*instance, error) {
	select {
	case <-p.m.done:
		return nil, ErrClosed
	case inst := <-p.idle:
		return inst, nil
	default:
	}
	select {
	case inst := <-p.idle:
		return inst, nil
	case p.slots <- struct{}{}:
		inst, err := p.m.newInstance(context.Background())
		if err != nil {
			<-p.slots
			return nil, err
		}
		return inst, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.m.done:
		return nil, ErrClosed
	}
}

func (p *pool) discard(inst *instance) {
	inst.mod.Close(context.Background())
	<-p.slots
}

func (p *pool) put(inst *instance) {
	p.m.mu.Lock()
	closed := p.m.closed
	p.m.mu.Unlock()
	if closed {
		inst.mod.Close(context.Background())
		return
	}
	p.idle <- inst
}

func (m *Module) call(ctx context.Context, p *pool, count uint64, pid uint32, ts, x int64) ([]byte, error) {
	inst, err := p.get(ctx)
	if err != nil {
		return nil, err
	}
	inst.stack[0], inst.stack[1], inst.stack[2], inst.stack[3] = count, uint64(pid), uint64(ts), uint64(x)
	if err := inst.handle.CallWithStack(ctx, inst.stack); err != nil {
		p.discard(inst) // a trapped instance may be corrupt: never reuse it
		return nil, fmt.Errorf("guest trap: %w", err)
	}
	packed := inst.stack[0]
	body, ok := inst.mod.Memory().Read(uint32(packed>>32), uint32(packed))
	if !ok {
		p.discard(inst)
		return nil, errors.New("guest returned out-of-bounds buffer")
	}
	out := append([]byte(nil), body...) // copy out of linear memory
	p.put(inst)
	return out, nil
}

// Call serves a client-visible request.
func (m *Module) Call(ctx context.Context, count uint64, pid uint32, ts, x int64) ([]byte, error) {
	return m.call(ctx, m.serve, count, pid, ts, x)
}

// CallShadow serves a mirrored request on the isolated shadow pool.
func (m *Module) CallShadow(ctx context.Context, count uint64, pid uint32, ts, x int64) ([]byte, error) {
	return m.call(ctx, m.shadow, count, pid, ts, x)
}

// Close unloads the module. Only call it once no request can reach it.
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
	for _, p := range []*pool{m.serve, m.shadow} {
	drain:
		for {
			select {
			case inst := <-p.idle:
				inst.mod.Close(ctx)
			default:
				break drain
			}
		}
	}
	return m.rt.Close(ctx)
}
