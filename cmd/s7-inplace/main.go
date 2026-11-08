// Command s7-inplace is the S7 service: a contract-compliant microservice
// whose business logic is a WASM module that can be replaced IN PLACE by a
// signed patch, validated by an in-process shadow + canary rollout, and
// rolled back automatically. It is designed to be driven by the HotPatch
// Kubernetes operator in operator/ (see docs/solutions/S7-k8s-inplace-patch-operator.md).
//
//	./cmd/s7-inplace/build.sh
//	./cmd/s7-inplace/bin/s7-inplace -addr 127.0.0.1:8080        # loopback-only: no token needed
//	curl -XPOST 'localhost:8080/admin/upgrade?to=v2'         # contract harness (pre-staged signed bundle)
//	s7-patchctl push -bundle bundles/v3 -url http://localhost:9090   # signed patch upload (admin port)
//
// Endpoints on -addr: /api, /healthz, /admin/status and (with
// -contract-upgrade) the privileged /admin/upgrade. On -admin-addr (default
// 127.0.0.1:9090, "none" disables it; never the public port): POST
// /admin/patch, GET /admin/rollout, POST /admin/rollout/abort, POST
// /admin/rollback.
//
// Admin authentication fails closed: if a listener serving privileged
// endpoints is not bound to a loopback address, the process refuses to
// start without -admin-token-file, unless -insecure-admin is given.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/canary"
	"github.com/nareknaltakyan1/dimp-research/internal/canary/wasmguest"
	"github.com/nareknaltakyan1/dimp-research/internal/signing"
)

func exeDir(sub string) string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), sub)
	}
	return sub
}

func main() {
	addr := flag.String("addr", ":8080", "listen address (contract endpoints)")
	adminAddr := flag.String("admin-addr", "127.0.0.1:9090", "separate listen address for privileged admin endpoints (\"none\" disables them; they are never served on -addr)")
	moduleDir := flag.String("module-dir", exeDir("modules"), "directory with built-in <version>.wasm modules (trusted via the image)")
	initial := flag.String("initial", "v1", "initial built-in module")
	bundleDir := flag.String("bundle-dir", exeDir("bundles"), "pre-staged signed bundles for /admin/upgrade?to=<v>")
	keys := flag.String("trusted-keys", exeDir("keys"), "comma-separated public key files or directories (PEM ed25519)")
	tokenFile := flag.String("admin-token-file", "", "file containing the bearer token required on admin endpoints")
	service := flag.String("service", "s7-inplace", "service name patches must target")
	schema := flag.String("state-schema", "counter/v1", "host state schema patches must declare")
	contract := flag.Bool("contract-upgrade", true, "enable POST /admin/upgrade (loads pre-staged signed bundles only)")
	pool := flag.Int("pool", 8, "WASM instances per module and pool")
	insecure := flag.Bool("insecure-admin", false, "allow privileged endpoints without a token on NON-loopback listeners (never in production)")
	stateDir := flag.String("state-dir", "", "directory for the persisted anti-rollback high-water mark (empty = in memory only)")
	maxAge := flag.Duration("max-bundle-age", 30*24*time.Hour, "reject bundles whose signed creation time is older (0 = no limit)")
	flag.Parse()
	if *adminAddr == "none" {
		*adminAddr = ""
	}

	keyList, err := signing.LoadPublicKeys(splitList(*keys)...)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("trusted keys: %v", err)
	}
	token := ""
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatalf("admin token: %v", err)
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			log.Fatalf("admin token file %s is empty", *tokenFile)
		}
	}
	// Fail closed: every listener that serves privileged endpoints must be
	// loopback-only unless a token is configured (or -insecure-admin).
	var privileged []string
	if *adminAddr != "" {
		privileged = append(privileged, *adminAddr)
	}
	if *contract {
		privileged = append(privileged, *addr)
	}
	tokenless := false
	if token == "" {
		for _, a := range privileged {
			if !isLoopback(a) && !*insecure {
				log.Fatalf("refusing to serve admin endpoints on %s without -admin-token-file: bind it to a loopback address, disable it (-admin-addr=none / -contract-upgrade=false) or pass -insecure-admin", a)
			}
		}
		tokenless = true
	}
	s, err := NewServer(Config{
		Service:         *service,
		StateSchema:     *schema,
		ModuleDir:       *moduleDir,
		Initial:         *initial,
		BundleDir:       *bundleDir,
		Verifier:        signing.NewVerifier(keyList...),
		AdminToken:      token,
		AllowTokenless:  tokenless,
		ContractUpgrade: *contract,
		StateDir:        *stateDir,
		MaxBundleAge:    *maxAge,
		Wasm:            wasmguest.Options{PoolSize: *pool},
		UpgradeStrategy: canary.FastStrategy(),
	})
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	ids := s.cfg.Verifier.KeyIDs()
	s.log.Emit("listening", "addr", *addr, "admin_addr", *adminAddr, "version", *initial, "trusted_keys", ids, "admin_token", token != "", "tokenless_admin", tokenless, "insecure_admin", *insecure, "state_dir", *stateDir)

	srv := &http.Server{Addr: *addr, Handler: s.Handler(false), ReadHeaderTimeout: 10 * time.Second}
	var admin *http.Server
	if *adminAddr != "" {
		admin = &http.Server{Addr: *adminAddr, Handler: s.AdminHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigs {
			if sig == syscall.SIGHUP {
				go s.Upgrade("")
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			srv.Shutdown(ctx)
			if admin != nil {
				admin.Shutdown(ctx)
			}
			cancel()
			return
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	s.Close()
}

// isLoopback reports whether a listen address is explicitly bound to a
// loopback interface ("127.0.0.1:p", "[::1]:p", "localhost:p"). An empty
// host (":8080") listens on all interfaces and is NOT loopback.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if _, err := os.Stat(p); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}
