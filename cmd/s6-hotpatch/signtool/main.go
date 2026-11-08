// Command signtool creates ed25519 keys and signed hotpatch manifests for
// s6-hotpatch (see internal/patchable/delivery).
//
//	signtool keygen -priv keys/signing.key -pub keys/trusted.pub
//	signtool sign -key keys/signing.key -so plugins/v2.so -label v2 -serial N [-ttl 720h] \
//	    -fingerprints v2.fp.json [-only sym,sym] [-plugin-name v2.so] -out plugins/v2.manifest.json
//
// -serial must increase with every new patch (the loader rejects serials
// below the highest one it applied); build.sh uses the build time.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/patchable/delivery"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: signtool keygen|sign ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "signtool:", err)
		os.Exit(1)
	}
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	priv := fs.String("priv", "signing.key", "private key (seed hex) output")
	pub := fs.String("pub", "trusted.pub", "public key (hex) output")
	fs.Parse(args)
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(*priv), 0o700)
	os.MkdirAll(filepath.Dir(*pub), 0o755)
	if err := delivery.WriteKeyPair(*priv, *pub, k); err != nil {
		return err
	}
	fmt.Printf("key_id %s\n", delivery.KeyID(k.Public().(ed25519.PublicKey)))
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "", "private key file")
	so := fs.String("so", "", "plugin file to hash")
	label := fs.String("label", "", "patch-set label (e.g. v2)")
	fps := fs.String("fingerprints", "", "JSON from `hotpatchgen fingerprints`")
	only := fs.String("only", "", "comma-separated subset of symbols to include")
	name := fs.String("plugin-name", "", "plugin file name recorded in the manifest (default: base name of -so)")
	out := fs.String("out", "", "manifest output path")
	serial := fs.Uint64("serial", 0, "monotonic patch serial (> 0, must exceed every earlier patch's)")
	ttl := fs.Duration("ttl", 30*24*time.Hour, "manifest validity from now")
	fs.Parse(args)
	if *keyPath == "" || *so == "" || *label == "" || *fps == "" || *out == "" || *serial == 0 {
		return fmt.Errorf("sign: -key, -so, -label, -serial, -fingerprints and -out are required")
	}
	if *ttl <= 0 {
		return fmt.Errorf("sign: -ttl must be positive")
	}
	k, err := delivery.ReadPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	sum, err := delivery.HashFile(*so)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(*fps)
	if err != nil {
		return err
	}
	var syms map[string]delivery.Symbol
	if err := json.Unmarshal(b, &syms); err != nil {
		return err
	}
	if *only != "" {
		keep := map[string]delivery.Symbol{}
		for _, s := range strings.Split(*only, ",") {
			v, ok := syms[s]
			if !ok {
				return fmt.Errorf("-only: %s not in %s", s, *fps)
			}
			keep[s] = v
		}
		syms = keep
	}
	if *name == "" {
		*name = filepath.Base(*so)
	}
	m := delivery.Manifest{Format: delivery.Format, Label: *label, Serial: *serial,
		ExpiresUnix: time.Now().Add(*ttl).Unix(), Plugin: *name, SHA256: sum, Symbols: syms}
	m.Sign(k)
	mb, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(*out, append(mb, '\n'), 0o644)
}
