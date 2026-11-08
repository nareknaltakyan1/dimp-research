package delivery

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nareknaltakyan1/dimp-research/internal/patchable"
)

func impl(ctx context.Context, x int) string { return "" }

func setup(t *testing.T) (*patchable.Registry, ed25519.PrivateKey, string, Manifest) {
	reg := patchable.NewRegistry("v1")
	s := reg.MustRegister(patchable.SlotSpec{Symbol: "p.F", Impl: impl})
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	so := filepath.Join(dir, "x.so")
	os.WriteFile(so, []byte("not really a plugin"), 0o644)
	sum, _ := HashFile(so)
	m := Manifest{Format: Format, Label: "v2", Plugin: "x.so", SHA256: sum,
		Symbols: map[string]Symbol{"p.F": {Fingerprint: s.Fingerprint, Signature: s.Signature}}}
	return reg, priv, dir, m
}

func write(t *testing.T, dir string, m Manifest) {
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "x.manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSignVerify(t *testing.T) {
	_, priv, _, m := setup(t)
	pub := priv.Public().(ed25519.PublicKey)
	m.Sign(priv)
	if err := m.Verify([]ed25519.PublicKey{pub}); err != nil {
		t.Fatal(err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if err := m.Verify([]ed25519.PublicKey{other.Public().(ed25519.PublicKey)}); !errors.Is(err, ErrUntrustedKey) {
		t.Fatalf("untrusted: %v", err)
	}
	for name, mut := range map[string]func(*Manifest){
		"label":  func(m *Manifest) { m.Label = "v3" },
		"hash":   func(m *Manifest) { m.SHA256 = strings.Repeat("0", 64) },
		"symbol": func(m *Manifest) { m.Symbols["p.G"] = Symbol{Fingerprint: "x"} },
	} {
		c := m
		c.Symbols = map[string]Symbol{}
		for k, v := range m.Symbols {
			c.Symbols[k] = v
		}
		mut(&c)
		if err := c.Verify([]ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s tamper not detected: %v", name, err)
		}
	}
}

func TestLoaderRejectsBeforeOpen(t *testing.T) {
	reg, priv, dir, m := setup(t)
	pub := priv.Public().(ed25519.PublicKey)
	l := &Loader{Reg: reg, Trusted: []ed25519.PublicKey{pub}, WorkDir: t.TempDir()}

	// fingerprint mismatch -> rejected before the hash/open stages
	bad := m
	bad.Symbols = map[string]Symbol{"p.F": {Fingerprint: "0123456789abcdef", Signature: "func(int)"}}
	bad.Sign(priv)
	write(t, dir, bad)
	if _, _, _, err := l.Load(dir, "x"); !errors.Is(err, patchable.ErrSignature) || !strings.Contains(err.Error(), "pre-load") {
		t.Fatalf("fingerprint: %v", err)
	}
	// unknown symbol
	unk := m
	unk.Symbols = map[string]Symbol{"p.Nope": {Fingerprint: "0123456789abcdef"}}
	unk.Sign(priv)
	write(t, dir, unk)
	if _, _, _, err := l.Load(dir, "x"); !errors.Is(err, patchable.ErrUnknownSymbol) {
		t.Fatalf("unknown: %v", err)
	}
	// hash mismatch (file changed after signing)
	m.Sign(priv)
	write(t, dir, m)
	os.WriteFile(filepath.Join(dir, "x.so"), []byte("modified"), 0o644)
	if _, _, _, err := l.Load(dir, "x"); !errors.Is(err, ErrHash) {
		t.Fatalf("hash: %v", err)
	}
	// untrusted signer
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	m.Sign(other)
	write(t, dir, m)
	if _, _, _, err := l.Load(dir, "x"); !errors.Is(err, ErrUntrustedKey) {
		t.Fatalf("untrusted: %v", err)
	}
}
