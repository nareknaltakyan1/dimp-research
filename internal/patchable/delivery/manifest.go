// Package delivery loads hotpatches shipped as Go plugins, guarded by an
// ed25519-signed manifest.
//
// A patch is two files:
//
//	<name>.so             go build -buildmode=plugin; exports `var Patches map[string]any`
//	<name>.manifest.json  {format, label, plugin, sha256, symbols{sym:{fingerprint,signature}}, key_id, sig}
//
// Load order (each step fails closed, nothing is applied on failure):
//
//  1. parse manifest, check the key is trusted and the ed25519 signature
//     over the canonical manifest bytes is valid;
//  2. check every manifest symbol exists in the registry with the same
//     signature fingerprint -- BEFORE dlopen, because plugin.Open runs the
//     plugin's init code and can never be undone;
//  3. copy the .so into a private directory while hashing it and compare
//     with the signed sha256 (the copy is what gets opened, closing the
//     check-then-open race on the original path);
//  4. plugin.Open, look up Patches, check the exported set equals the
//     manifest's set and each value's run-time fingerprint matches;
//  5. hand the map to Registry.Apply (one transaction).
package delivery

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Format identifies the manifest schema.
const Format = "dimp-hotpatch/v1"

// Symbol is the expected signature of one replacement.
type Symbol struct {
	Fingerprint string `json:"fingerprint"`
	Signature   string `json:"signature"`
}

// Manifest describes a signed patch.
type Manifest struct {
	Format  string            `json:"format"`
	Label   string            `json:"label"`
	Plugin  string            `json:"plugin"` // file name, relative to the manifest
	SHA256  string            `json:"sha256"`
	Symbols map[string]Symbol `json:"symbols"`
	KeyID   string            `json:"key_id"`
	Sig     string            `json:"sig,omitempty"` // base64 ed25519 over SigningBytes()
}

// SigningBytes is the canonical byte string that is signed: the JSON
// encoding of the manifest with Sig cleared (encoding/json sorts map keys,
// so the encoding is deterministic).
func (m Manifest) SigningBytes() []byte {
	m.Sig = ""
	b, _ := json.Marshal(m)
	return append([]byte("dimp-hotpatch-manifest\x00"), b...)
}

// KeyID derives a short identifier for a public key.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// Sign fills KeyID and Sig.
func (m *Manifest) Sign(priv ed25519.PrivateKey) {
	m.KeyID = KeyID(priv.Public().(ed25519.PublicKey))
	m.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.SigningBytes()))
}

// Errors.
var (
	ErrUntrustedKey = errors.New("delivery: manifest signed by an untrusted key")
	ErrBadSignature = errors.New("delivery: bad manifest signature")
	ErrHash         = errors.New("delivery: plugin hash does not match manifest")
	ErrFormat       = errors.New("delivery: bad manifest")
)

// Verify checks format and signature against the trusted keys.
func (m Manifest) Verify(trusted []ed25519.PublicKey) error {
	if m.Format != Format || m.Plugin == "" || len(m.SHA256) != 64 || len(m.Symbols) == 0 {
		return ErrFormat
	}
	sig, err := base64.StdEncoding.DecodeString(m.Sig)
	if err != nil {
		return ErrBadSignature
	}
	for _, k := range trusted {
		if KeyID(k) != m.KeyID {
			continue
		}
		if !ed25519.Verify(k, m.SigningBytes(), sig) {
			return ErrBadSignature
		}
		return nil
	}
	return fmt.Errorf("%w (key_id %s)", ErrUntrustedKey, m.KeyID)
}

// ReadManifest parses a manifest file.
func ReadManifest(path string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	return m, nil
}

// Key files hold hex: the 32-byte seed (private) or the 32-byte public key.

// WriteKeyPair writes priv (seed hex) and pub (hex) files.
func WriteKeyPair(privPath, pubPath string, priv ed25519.PrivateKey) error {
	if err := os.WriteFile(privPath, []byte(hex.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(pubPath, []byte(hex.EncodeToString(priv.Public().(ed25519.PublicKey))+"\n"), 0o644)
}

// ReadPrivateKey reads a seed-hex file.
func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("bad private key file %s", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ReadPublicKeys reads one hex public key per non-empty line.
func ReadPublicKeys(path string) ([]ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []ed25519.PublicKey
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		k, err := hex.DecodeString(ln)
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("bad public key in %s", path)
		}
		keys = append(keys, ed25519.PublicKey(k))
	}
	return keys, nil
}
