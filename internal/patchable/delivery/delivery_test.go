package delivery

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"plugin"
	"strings"
	"testing"
	"time"

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
	m := Manifest{Format: Format, Label: "v2", Serial: 10, ExpiresUnix: time.Now().Add(time.Hour).Unix(), Plugin: "x.so", SHA256: sum,
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
		"serial": func(m *Manifest) { m.Serial = 99 },
		"expiry": func(m *Manifest) { m.ExpiresUnix += 3600 },
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

func TestManifestRequiresSerialAndExpiry(t *testing.T) {
	_, priv, _, m := setup(t)
	pub := []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)}
	for name, mut := range map[string]func(*Manifest){
		"no serial": func(m *Manifest) { m.Serial = 0 },
		"no expiry": func(m *Manifest) { m.ExpiresUnix = 0 },
		"v1 format": func(m *Manifest) { m.Format = "dimp-hotpatch/v1" },
	} {
		c := m
		mut(&c)
		c.Sign(priv)
		if err := c.Verify(pub); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: want ErrFormat, got %v", name, err)
		}
	}
}

// Regression (review, Low): expired, replayed and downgraded manifests are
// rejected before the plugin is copied or opened.
func TestLoaderRejectsExpiredReplayDowngrade(t *testing.T) {
	reg, priv, dir, m := setup(t)
	pub := priv.Public().(ed25519.PublicKey)
	state := filepath.Join(t.TempDir(), "state", "hwm.json")
	work := t.TempDir()
	l := &Loader{Reg: reg, Trusted: []ed25519.PublicKey{pub}, WorkDir: work, StateFile: state}

	exp := m
	exp.ExpiresUnix = time.Now().Add(-time.Second).Unix()
	exp.Sign(priv)
	write(t, dir, exp)
	if _, _, _, err := l.Load(dir, "x"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}

	// Pretend serial 10 (this .so) was applied.
	if err := l.Commit(Report{Serial: 10, SHA256: m.SHA256}); err != nil {
		t.Fatal(err)
	}
	old := m
	old.Serial = 9
	old.Sign(priv)
	write(t, dir, old)
	if _, _, _, err := l.Load(dir, "x"); !errors.Is(err, ErrReplay) {
		t.Fatalf("downgrade: %v", err)
	}
	reuse := m
	reuse.SHA256 = strings.Repeat("ab", 32) // same serial, different content
	reuse.Sign(priv)
	write(t, dir, reuse)
	if _, _, _, err := l.Load(dir, "x"); !errors.Is(err, ErrReplay) {
		t.Fatalf("serial reuse: %v", err)
	}
	if ents, _ := os.ReadDir(work); len(ents) != 0 {
		t.Fatalf("rejected manifests must not reach the copy stage: %v", ents)
	}
	// Same serial + same hash (re-apply after a local rollback) passes the
	// freshness check (and then fails later only because x.so is no plugin).
	m.Sign(priv)
	write(t, dir, m)
	if _, _, _, err := l.Load(dir, "x"); errors.Is(err, ErrReplay) || errors.Is(err, ErrExpired) {
		t.Fatalf("re-apply of the committed patch rejected: %v", err)
	}
	if err := l.Commit(Report{Serial: 9, SHA256: m.SHA256}); !errors.Is(err, ErrReplay) {
		t.Fatalf("commit lowered the high-water mark: %v", err)
	}
	// The mark survives a restart (new Loader, same state file).
	l2 := &Loader{Reg: reg, Trusted: []ed25519.PublicKey{pub}, WorkDir: t.TempDir(), StateFile: state}
	if hwm, err := l2.HighWaterMark(); err != nil || hwm != 10 {
		t.Fatalf("persisted hwm = %d, %v", hwm, err)
	}
	write(t, dir, old)
	if _, _, _, err := l2.Load(dir, "x"); !errors.Is(err, ErrReplay) {
		t.Fatalf("downgrade after restart: %v", err)
	}
	// A corrupt state file fails closed.
	os.WriteFile(state, []byte("{"), 0o600)
	l3 := &Loader{Reg: reg, Trusted: []ed25519.PublicKey{pub}, StateFile: state}
	m.Serial = 11
	m.Sign(priv)
	write(t, dir, m)
	if _, _, _, err := l3.Load(dir, "x"); err == nil || !strings.Contains(err.Error(), "corrupt state") {
		t.Fatalf("corrupt state: %v", err)
	}
}

// Regression (review, Low): when the plugin with this hash is already mapped
// (upgrade -> rollback -> upgrade), the fresh private copy must be deleted.
func TestCachedPluginDoesNotLeakCopy(t *testing.T) {
	reg, priv, dir, m := setup(t)
	pub := priv.Public().(ed25519.PublicKey)
	work := t.TempDir()
	l := &Loader{Reg: reg, Trusted: []ed25519.PublicKey{pub}, WorkDir: work}
	l.opened = map[string]*plugin.Plugin{m.SHA256: new(plugin.Plugin)} // "already mapped"
	m.Sign(priv)
	write(t, dir, m)
	for i := 0; i < 3; i++ {
		_, _, rep, err := l.Load(dir, "x")
		if !rep.CachedPlugin || err == nil { // the zero Plugin has no Patches symbol
			t.Fatalf("expected cached open then lookup error: cached=%v err=%v", rep.CachedPlugin, err)
		}
	}
	if ents, _ := os.ReadDir(work); len(ents) != 0 {
		t.Fatalf("private copies leaked: %d files", len(ents))
	}
}
