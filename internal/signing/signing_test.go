package signing

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

var module = bytes.Repeat([]byte("\x00asm-pretend-module-"), 1000)

func manifest(v, base string) Manifest {
	return Manifest{Name: "s7-inplace", Version: v, BaseVersion: base, CompatibleStateSchema: "counter/v1"}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := mustKey(t)
	b, err := Sign(priv, manifest("v2", "v1"), module)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier(pub)
	m, err := v.Verify(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v2" || m.SHA256 != Digest(module) || m.Size != int64(len(module)) {
		t.Fatalf("bad manifest %+v", m)
	}
	pol := Policy{Service: "s7-inplace", StateSchema: "counter/v1"}
	if err := pol.Check(m, "v1", CheckOptions{}); err != nil {
		t.Fatal(err)
	}

	// Envelope round trip.
	enc, err := b.Encode()
	if err != nil {
		t.Fatal(err)
	}
	b2, err := DecodeEnvelope(enc, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(b2); err != nil {
		t.Fatal(err)
	}
	// Directory round trip.
	dir := filepath.Join(t.TempDir(), "b")
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	b3, err := ReadDir(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(b3); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedModule(t *testing.T) {
	pub, priv := mustKey(t)
	b, _ := Sign(priv, manifest("v2", "v1"), module)
	evil := append([]byte(nil), b.Module...)
	evil[100] ^= 0xff
	b.Module = evil
	if _, err := NewVerifier(pub).Verify(b); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	// Substituting a whole different module (same size or not) also fails.
	b.Module = []byte("other code")
	if _, err := NewVerifier(pub).Verify(b); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
}

func TestTamperedManifest(t *testing.T) {
	pub, priv := mustKey(t)
	b, _ := Sign(priv, manifest("v2", "v1"), module)
	// Attacker bumps the version to bypass the downgrade check.
	var m map[string]any
	json.Unmarshal(b.Manifest, &m)
	m["version"] = "v99"
	b.Manifest, _ = json.Marshal(m)
	if _, err := NewVerifier(pub).Verify(b); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestTamperedSignatureAndTruncation(t *testing.T) {
	pub, priv := mustKey(t)
	b, _ := Sign(priv, manifest("v2", "v1"), module)
	b.Signature[0] ^= 1
	if _, err := NewVerifier(pub).Verify(b); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
	b.Signature = b.Signature[:10]
	if _, err := NewVerifier(pub).Verify(b); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestWrongKey(t *testing.T) {
	pub, _ := mustKey(t)
	_, evilPriv := mustKey(t)
	b, _ := Sign(evilPriv, manifest("v2", "v1"), module)
	if _, err := NewVerifier(pub).Verify(b); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
	// Attacker claims the trusted key id but signs with their own key.
	b.KeyID = KeyID(pub)
	if _, err := NewVerifier(pub).Verify(b); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestReplayAndDowngrade(t *testing.T) {
	pub, priv := mustKey(t)
	v := NewVerifier(pub)
	pol := Policy{Service: "s7-inplace", StateSchema: "counter/v1"}
	b1, _ := Sign(priv, manifest("v1", ""), module)
	b2, _ := Sign(priv, manifest("v2", "v1"), module)
	m1, _ := v.Verify(b1)
	m2, _ := v.Verify(b2)

	// Running v2: replaying the (validly signed) v2 bundle is rejected.
	if err := pol.Check(m2, "v2", CheckOptions{}); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("replay: want ErrDowngrade, got %v", err)
	}
	// Running v2: downgrade to v1 is rejected without explicit rollback ...
	if err := pol.Check(m1, "v2", CheckOptions{}); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("downgrade: want ErrDowngrade, got %v", err)
	}
	// There is no "rollback" admission at all: an older signed bundle can
	// never be admitted, whatever the caller claims (review 2026-11-08 High 1).
	f := &Floor{Version: "v2", Digest: m2.SHA256, Restorable: true}
	// Restart case: the process came back with image code v1 but had promoted
	// v2: the identical v2 bundle may be re-installed ...
	if err := pol.Check(m2, "v1", CheckOptions{Floor: f}); err != nil {
		t.Fatalf("restore of promoted floor: %v", err)
	}
	// ... but not once v2 was rolled back (floor no longer restorable) ...
	if err := pol.Check(m2, "v1", CheckOptions{Floor: &Floor{Version: "v2", Digest: m2.SHA256}}); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("replay after rollback: want ErrDowngrade, got %v", err)
	}
	// ... nor different code under the same version ...
	if err := pol.Check(m2, "v1", CheckOptions{Floor: &Floor{Version: "v2", Digest: "00", Restorable: true}}); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("same version, other digest: want ErrDowngrade, got %v", err)
	}
	// ... and nothing below the floor, even with Reconcile.
	if err := pol.Check(m2, "v1", CheckOptions{Reconcile: true, Floor: &Floor{Version: "v3"}}); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("below floor: want ErrDowngrade, got %v", err)
	}
	// Semantic version ordering, not lexical: v10 > v9.
	if c, _ := CompareVersions("v10", "v9"); c != 1 {
		t.Fatal("v10 should be > v9")
	}
	if c, _ := CompareVersions("v2.1", "v2.0.9"); c != 1 {
		t.Fatal("v2.1 should be > v2.0.9")
	}
}

func TestPolicyTargetSchemaBaseAge(t *testing.T) {
	pub, priv := mustKey(t)
	v := NewVerifier(pub)
	pol := Policy{Service: "s7-inplace", StateSchema: "counter/v1"}

	other := manifest("v2", "v1")
	other.Name = "payments"
	b, _ := Sign(priv, other, module)
	m, _ := v.Verify(b)
	if err := pol.Check(m, "v1", CheckOptions{}); !errors.Is(err, ErrWrongTarget) {
		t.Fatalf("want ErrWrongTarget, got %v", err)
	}

	schema := manifest("v2", "v1")
	schema.CompatibleStateSchema = "counter/v2"
	b, _ = Sign(priv, schema, module)
	m, _ = v.Verify(b)
	if err := pol.Check(m, "v1", CheckOptions{}); !errors.Is(err, ErrSchema) {
		t.Fatalf("want ErrSchema, got %v", err)
	}

	b, _ = Sign(priv, manifest("v3", "v2"), module)
	m, _ = v.Verify(b)
	if err := pol.Check(m, "v1", CheckOptions{}); !errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("want ErrBaseMismatch, got %v", err)
	}
	if err := pol.Check(m, "v1", CheckOptions{Reconcile: true}); err != nil {
		t.Fatalf("reconcile should allow base mismatch: %v", err)
	}

	old := manifest("v2", "v1")
	old.Created = time.Now().Add(-48 * time.Hour)
	b, _ = Sign(priv, old, module)
	m, _ = v.Verify(b)
	pol.MaxAge = 24 * time.Hour
	if err := pol.Check(m, "v1", CheckOptions{}); !errors.Is(err, ErrTooOld) {
		t.Fatalf("want ErrTooOld, got %v", err)
	}
	future := manifest("v2", "v1")
	future.Created = time.Now().Add(time.Hour)
	b, _ = Sign(priv, future, module)
	m, _ = v.Verify(b)
	if err := pol.Check(m, "v1", CheckOptions{}); !errors.Is(err, ErrTooOld) {
		t.Fatalf("future-dated: want ErrTooOld, got %v", err)
	}
	b, _ = Sign(priv, manifest("v2", "v1"), module)
	m, _ = v.Verify(b)
	if err := pol.Check(m, "v1", CheckOptions{}); err != nil {
		t.Fatalf("fresh bundle with MaxAge: %v", err)
	}
}

func TestZipBombAndSizeLimits(t *testing.T) {
	pub, priv := mustKey(t)
	big := make([]byte, 4<<20)
	b, _ := Sign(priv, manifest("v2", "v1"), big)
	enc, _ := b.Encode()
	if _, err := DecodeEnvelope(enc, 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	v := NewVerifier(pub)
	v.MaxModuleSize = 1 << 20
	if _, err := v.Verify(b); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if _, err := DecodeEnvelope([]byte("{not json"), 0); !errors.Is(err, ErrMalformedBundle) {
		t.Fatalf("want ErrMalformedBundle, got %v", err)
	}
}

func TestKeyFiles(t *testing.T) {
	dir := t.TempDir()
	pub, priv := mustKey(t)
	if err := WritePrivateKey(filepath.Join(dir, "k.key"), priv); err != nil {
		t.Fatal(err)
	}
	if err := WritePublicKey(filepath.Join(dir, "k.pub"), pub); err != nil {
		t.Fatal(err)
	}
	p2, err := ReadPrivateKey(filepath.Join(dir, "k.key"))
	if err != nil || !p2.Equal(priv) {
		t.Fatalf("private key round trip: %v", err)
	}
	keys, err := LoadPublicKeys(dir)
	if err != nil || len(keys) != 1 || !keys[0].Equal(pub) {
		t.Fatalf("public key load: %v %d", err, len(keys))
	}
}

func BenchmarkVerify(b *testing.B) {
	pub, priv, _ := GenerateKey()
	mod := make([]byte, 1600000) // size of a Go wasip1 guest
	bd, _ := Sign(priv, manifest("v2", "v1"), mod)
	v := NewVerifier(pub)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.Verify(bd); err != nil {
			b.Fatal(err)
		}
	}
}
