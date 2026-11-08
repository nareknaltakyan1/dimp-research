// Command s7-patchctl manages signed S7 hot-patch bundles.
//
//	s7-patchctl keygen  -out keys/release            # keys/release.key (0600) + keys/release.pub
//	s7-patchctl sign    -key keys/release.key -module v2.wasm -version v2 -base v1 -out bundles/v2
//	s7-patchctl verify  -keys keys/release.pub -bundle bundles/v2 [-current v1] [-rollback] [-reconcile]
//	s7-patchctl envelope -bundle bundles/v2 -out v2.json   # single-file JSON envelope (POST body)
//	s7-patchctl push    -bundle bundles/v2 -url http://pod:9090 [-token-file f] [-strategy fast] [-reconcile] [-rollback]
//	s7-patchctl status  -url http://pod:9090 [-token-file f]
//	s7-patchctl abort   -url http://pod:9090 [-token-file f]
//	s7-patchctl tamper  -bundle bundles/v2 -module evil.wasm -out bundles/evil   # demo: substitute module, keep signature
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/signing"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: s7-patchctl keygen|sign|verify|envelope|push|status|abort|tamper [flags]")
	os.Exit(2)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "s7-patchctl: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "keygen":
		out := fs.String("out", "patch-signing", "output path prefix (<out>.key, <out>.pub)")
		fs.Parse(args)
		pub, priv, err := signing.GenerateKey()
		if err != nil {
			die("%v", err)
		}
		os.MkdirAll(filepath.Dir(*out), 0o755)
		if err := signing.WritePrivateKey(*out+".key", priv); err != nil {
			die("%v", err)
		}
		if err := signing.WritePublicKey(*out+".pub", pub); err != nil {
			die("%v", err)
		}
		fmt.Printf("key_id=%s private=%s.key public=%s.pub\n", signing.KeyID(pub), *out, *out)

	case "sign":
		key := fs.String("key", "", "private key (PEM)")
		module := fs.String("module", "", "WASM module")
		name := fs.String("name", "s7-inplace", "target service")
		version := fs.String("version", "", "patch version (e.g. v2)")
		base := fs.String("base", "", "base version")
		schema := fs.String("schema", "counter/v1", "compatible state schema")
		out := fs.String("out", "", "output bundle directory")
		fs.Parse(args)
		if *key == "" || *module == "" || *version == "" || *out == "" {
			die("sign: -key, -module, -version and -out are required")
		}
		priv, err := signing.ReadPrivateKey(*key)
		if err != nil {
			die("%v", err)
		}
		wasm, err := os.ReadFile(*module)
		if err != nil {
			die("%v", err)
		}
		b, err := signing.Sign(priv, signing.Manifest{Name: *name, Version: *version, BaseVersion: *base, CompatibleStateSchema: *schema}, wasm)
		if err != nil {
			die("%v", err)
		}
		if err := b.WriteDir(*out); err != nil {
			die("%v", err)
		}
		fmt.Printf("signed %s: %s\n", *out, b.Manifest)

	case "verify":
		keys := fs.String("keys", "", "comma-separated trusted public keys / dirs")
		bundle := fs.String("bundle", "", "bundle directory")
		current := fs.String("current", "", "running version (enables version policy check)")
		name := fs.String("name", "", "required target service")
		rollback := fs.Bool("rollback", false, "explicit rollback")
		reconcile := fs.Bool("reconcile", false, "allow base mismatch")
		fs.Parse(args)
		pubs, err := signing.LoadPublicKeys(strings.Split(*keys, ",")...)
		if err != nil {
			die("%v", err)
		}
		b, err := signing.ReadDir(*bundle, 0)
		if err != nil {
			die("%v", err)
		}
		t0 := time.Now()
		m, err := signing.NewVerifier(pubs...).Verify(b)
		if err != nil {
			die("REJECTED: %v", err)
		}
		if *current != "" {
			if err := (signing.Policy{Service: *name}).Check(m, *current, signing.CheckOptions{Rollback: *rollback, Reconcile: *reconcile}); err != nil {
				die("REJECTED: %v", err)
			}
		}
		fmt.Printf("OK %s %s sha256=%s verify=%v\n", m.Name, m.Version, m.SHA256, time.Since(t0))

	case "envelope":
		bundle := fs.String("bundle", "", "bundle directory")
		out := fs.String("out", "-", "output file")
		fs.Parse(args)
		b, err := signing.ReadDir(*bundle, 0)
		if err != nil {
			die("%v", err)
		}
		enc, _ := b.Encode()
		write(*out, enc)

	case "tamper":
		bundle := fs.String("bundle", "", "signed bundle directory")
		module := fs.String("module", "", "substitute module")
		out := fs.String("out", "", "output bundle directory")
		fs.Parse(args)
		b, err := signing.ReadDir(*bundle, 0)
		if err != nil {
			die("%v", err)
		}
		wasm, err := os.ReadFile(*module)
		if err != nil {
			die("%v", err)
		}
		b.Module = wasm // manifest + signature unchanged
		if err := b.WriteDir(*out); err != nil {
			die("%v", err)
		}
		fmt.Printf("tampered bundle %s: module replaced (sha256 %s), signed manifest kept\n", *out, signing.Digest(wasm))

	case "push", "status", "abort":
		url := fs.String("url", "http://localhost:8080", "pod admin base URL")
		tokenFile := fs.String("token-file", "", "bearer token file")
		bundle := fs.String("bundle", "", "bundle directory (push)")
		strategy := fs.String("strategy", "default", "default|fast|immediate (push)")
		reconcile := fs.Bool("reconcile", false, "allow base mismatch (push)")
		rollback := fs.Bool("rollback", false, "explicit rollback (push)")
		fs.Parse(args)
		var req *http.Request
		switch cmd {
		case "push":
			b, err := signing.ReadDir(*bundle, 0)
			if err != nil {
				die("%v", err)
			}
			env, _ := b.Encode()
			body, _ := json.Marshal(map[string]any{"bundle": json.RawMessage(env), "strategy_name": *strategy, "reconcile": *reconcile, "rollback": *rollback})
			req, _ = http.NewRequest(http.MethodPost, *url+"/admin/patch", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		case "status":
			req, _ = http.NewRequest(http.MethodGet, *url+"/admin/rollout", nil)
		case "abort":
			req, _ = http.NewRequest(http.MethodPost, *url+"/admin/rollout/abort", nil)
		}
		if *tokenFile != "" {
			t, err := os.ReadFile(*tokenFile)
			if err != nil {
				die("%v", err)
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(t)))
		}
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			die("%v", err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		fmt.Printf("HTTP %d\n%s", resp.StatusCode, out)
		if resp.StatusCode >= 300 {
			os.Exit(1)
		}
	default:
		usage()
	}
}

func write(path string, b []byte) {
	if path == "-" {
		os.Stdout.Write(b)
		return
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		die("%v", err)
	}
}
