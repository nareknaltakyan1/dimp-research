// Package signing implements ed25519 signing and verification of hot-patch
// bundles for S7 (signed in-place upgrades driven by a Kubernetes operator).
//
// A bundle is
//
//	module    the WASM module bytes (the code that will run in the process)
//	manifest  canonical JSON {name, version, base_version, sha256, size,
//	          created, compatible_state_schema}; sha256 is the digest of the
//	          UNCOMPRESSED module, so the signature binds the exact code
//	signature a detached ed25519 signature over
//	          "dimp-patch-v1\x00" || manifest bytes (domain separated)
//	key_id    hex(sha256(public key))[:16], selects the trusted key
//
// Verification order (cheap and fail-closed): key lookup -> signature ->
// manifest parse -> digest of module -> policy (target name, state schema,
// max age, version strictly newer than running and than the persisted
// high-water mark). Nothing is compiled or executed before every check has
// passed. Rollback is never a bundle admission (see CheckOptions).
package signing

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SignatureContext is prepended to the manifest before signing so a
// signature over a patch manifest can never be confused with a signature
// over anything else made with the same key.
const SignatureContext = "dimp-patch-v1\x00"

// Manifest describes a patch.
type Manifest struct {
	// Name is the target service; a bundle for another service is rejected.
	Name string `json:"name"`
	// Version is the logical version the patch installs ("v2", "v2.1.0").
	Version string `json:"version"`
	// BaseVersion is the version the patch was built and validated against.
	BaseVersion string `json:"base_version"`
	// SHA256 is the hex digest of the uncompressed module.
	SHA256 string `json:"sha256"`
	// Size is the uncompressed module size in bytes.
	Size int64 `json:"size"`
	// Created is the signing time (informational; also allows a max-age policy).
	Created time.Time `json:"created"`
	// CompatibleStateSchema names the host state schema the module expects.
	CompatibleStateSchema string `json:"compatible_state_schema"`
}

// Bundle is a signed patch.
type Bundle struct {
	Manifest  []byte // exact signed bytes
	Signature []byte // ed25519 signature (64 bytes)
	KeyID     string
	Module    []byte // uncompressed WASM
}

// Errors returned by verification. Callers can distinguish them with errors.Is.
var (
	ErrUnknownKey      = errors.New("signing: unknown or untrusted key id")
	ErrBadSignature    = errors.New("signing: signature verification failed")
	ErrBadManifest     = errors.New("signing: malformed manifest")
	ErrDigestMismatch  = errors.New("signing: module digest does not match signed manifest")
	ErrWrongTarget     = errors.New("signing: bundle is for a different service")
	ErrSchema          = errors.New("signing: incompatible state schema")
	ErrDowngrade       = errors.New("signing: version is not newer than current (replay/downgrade)")
	ErrBaseMismatch    = errors.New("signing: base_version does not match running version")
	ErrTooOld          = errors.New("signing: bundle is older than the maximum allowed age")
	ErrTooLarge        = errors.New("signing: module exceeds size limit")
	ErrMalformedBundle = errors.New("signing: malformed bundle")
)

// KeyID returns the identifier of a public key.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// Digest returns the hex SHA-256 of b.
func Digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// GenerateKey creates a new ed25519 key pair.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// Sign builds a bundle for module. Digest, size and (if zero) creation time
// are filled in from the module.
func Sign(priv ed25519.PrivateKey, m Manifest, module []byte) (*Bundle, error) {
	if m.Name == "" || m.Version == "" {
		return nil, fmt.Errorf("%w: name and version are required", ErrBadManifest)
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return nil, err
	}
	m.SHA256 = Digest(module)
	m.Size = int64(len(module))
	if m.Created.IsZero() {
		m.Created = time.Now().UTC().Truncate(time.Second)
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	return &Bundle{
		Manifest:  mb,
		Signature: ed25519.Sign(priv, signedMessage(mb)),
		KeyID:     KeyID(pub),
		Module:    module,
	}, nil
}

func signedMessage(manifest []byte) []byte {
	msg := make([]byte, 0, len(SignatureContext)+len(manifest))
	msg = append(msg, SignatureContext...)
	return append(msg, manifest...)
}

// Verifier holds the trusted public keys.
type Verifier struct {
	keys map[string]ed25519.PublicKey
	// MaxModuleSize bounds the module size (default 64 MiB).
	MaxModuleSize int64
}

// NewVerifier creates a verifier trusting pubs.
func NewVerifier(pubs ...ed25519.PublicKey) *Verifier {
	v := &Verifier{keys: make(map[string]ed25519.PublicKey)}
	for _, p := range pubs {
		v.keys[KeyID(p)] = p
	}
	return v
}

// KeyIDs lists the trusted key ids.
func (v *Verifier) KeyIDs() []string {
	ids := make([]string, 0, len(v.keys))
	for id := range v.keys {
		ids = append(ids, id)
	}
	return ids
}

func (v *Verifier) maxSize() int64 {
	if v.MaxModuleSize > 0 {
		return v.MaxModuleSize
	}
	return 64 << 20
}

// Verify checks the signature and the module digest and returns the parsed
// manifest. It does not apply version policy (see Policy.Check).
func (v *Verifier) Verify(b *Bundle) (*Manifest, error) {
	if b == nil {
		return nil, ErrMalformedBundle
	}
	pub, ok := v.keys[b.KeyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, b.KeyID)
	}
	if len(b.Signature) != ed25519.SignatureSize || !ed25519.Verify(pub, signedMessage(b.Manifest), b.Signature) {
		return nil, ErrBadSignature
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b.Manifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadManifest, err)
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return nil, err
	}
	if int64(len(b.Module)) > v.maxSize() {
		return nil, ErrTooLarge
	}
	if m.Size != int64(len(b.Module)) || !strings.EqualFold(m.SHA256, Digest(b.Module)) {
		return nil, ErrDigestMismatch
	}
	return &m, nil
}

// Policy is the in-process admission policy for a verified manifest.
type Policy struct {
	Service     string        // required manifest name
	StateSchema string        // required compatible_state_schema ("" = any)
	MaxAge      time.Duration // 0 = no limit (production callers should set it)
	// MaxClockSkew bounds how far in the future manifest.created may lie when
	// MaxAge is set (default 5 min).
	MaxClockSkew time.Duration
	Now          func() time.Time
}

// Floor is the persisted anti-rollback high-water mark of a process: the
// highest version it ever promoted or refused (after analysis or an explicit
// rollback). A bundle must be strictly newer than Floor.Version; the single
// exception is re-installing the IDENTICAL code (same version and digest) of
// a Floor that was promoted and never rolled back (Restorable), which is how
// a restarted container that came back with the image's built-in code gets
// its in-place version again.
type Floor struct {
	Version    string `json:"version"`
	Digest     string `json:"digest,omitempty"`
	Restorable bool   `json:"restorable"`
}

// CheckOptions modify a single admission. There is deliberately NO option
// that admits an older version: rollback is not a bundle admission but a
// re-install of the artifact the process itself retained (see cmd/s7-inplace
// /admin/rollback), so a replayed old signed bundle can never be used to
// downgrade.
type CheckOptions struct {
	// Reconcile allows base_version != current. Used when re-patching a
	// fresh pod that still runs the image's built-in code (spec drift). It
	// relaxes ONLY the base_version chaining; monotonicity against current
	// and Floor is always enforced.
	Reconcile bool
	// Floor, if non-nil, is the persisted high-water mark (see Floor).
	Floor *Floor
}

// Check admits m for a process currently running version current.
func (p Policy) Check(m *Manifest, current string, o CheckOptions) error {
	if p.Service != "" && m.Name != p.Service {
		return fmt.Errorf("%w: bundle %q, service %q", ErrWrongTarget, m.Name, p.Service)
	}
	if p.StateSchema != "" && m.CompatibleStateSchema != p.StateSchema {
		return fmt.Errorf("%w: bundle %q, host %q", ErrSchema, m.CompatibleStateSchema, p.StateSchema)
	}
	if p.MaxAge > 0 {
		now := time.Now
		if p.Now != nil {
			now = p.Now
		}
		skew := p.MaxClockSkew
		if skew <= 0 {
			skew = 5 * time.Minute
		}
		t := now()
		if m.Created.IsZero() || t.Sub(m.Created) > p.MaxAge {
			return fmt.Errorf("%w: created %s, max age %s", ErrTooOld, m.Created.Format(time.RFC3339), p.MaxAge)
		}
		if m.Created.Sub(t) > skew {
			return fmt.Errorf("%w: created %s is in the future", ErrTooOld, m.Created.Format(time.RFC3339))
		}
	}
	c, err := CompareVersions(m.Version, current)
	if err != nil {
		return err
	}
	if c <= 0 {
		return fmt.Errorf("%w: %s <= running %s", ErrDowngrade, m.Version, current)
	}
	if f := o.Floor; f != nil && f.Version != "" {
		cf, err := CompareVersions(m.Version, f.Version)
		if err != nil {
			return err
		}
		switch {
		case cf < 0:
			return fmt.Errorf("%w: %s < high-water mark %s", ErrDowngrade, m.Version, f.Version)
		case cf == 0 && !(f.Restorable && f.Digest != "" && strings.EqualFold(f.Digest, m.SHA256)):
			return fmt.Errorf("%w: %s was already installed or refused here (high-water mark %s, restorable=%v)", ErrDowngrade, m.Version, f.Version, f.Restorable)
		}
	}
	if !o.Reconcile && m.BaseVersion != "" && m.BaseVersion != current {
		return fmt.Errorf("%w: base %s, running %s", ErrBaseMismatch, m.BaseVersion, current)
	}
	return nil
}

// ParseVersion parses "vMAJOR[.MINOR[.PATCH]]" (leading v optional).
func ParseVersion(s string) ([3]int, error) {
	var out [3]int
	t := strings.TrimPrefix(s, "v")
	if t == "" {
		return out, fmt.Errorf("%w: empty version", ErrBadManifest)
	}
	parts := strings.Split(t, ".")
	if len(parts) > 3 {
		return out, fmt.Errorf("%w: bad version %q", ErrBadManifest, s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("%w: bad version %q", ErrBadManifest, s)
		}
		out[i] = n
	}
	return out, nil
}

// CompareVersions returns -1, 0, +1.
func CompareVersions(a, b string) (int, error) {
	va, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range va {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

// ---------------------------------------------------------------------------
// Wire and file formats.

// Envelope is the JSON transport form (POST /admin/patch). The module is
// gzip-compressed so a Go WASM module (~1.6 MB) fits a ConfigMap (~0.5 MB).
type Envelope struct {
	Manifest  []byte `json:"manifest"`  // base64 (encoding/json)
	Signature []byte `json:"signature"` // base64
	KeyID     string `json:"key_id"`
	ModuleGz  []byte `json:"module_gz"` // base64(gzip(module))
}

// Gzip compresses b.
func Gzip(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// Gunzip decompresses at most limit bytes (zip-bomb protection).
func Gunzip(b []byte, limit int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedBundle, err)
	}
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedBundle, err)
	}
	if int64(len(out)) > limit {
		return nil, ErrTooLarge
	}
	return out, nil
}

// Encode returns the JSON envelope for b.
func (b *Bundle) Encode() ([]byte, error) {
	return json.Marshal(Envelope{Manifest: b.Manifest, Signature: b.Signature, KeyID: b.KeyID, ModuleGz: Gzip(b.Module)})
}

// DecodeEnvelope parses an envelope; the module is decompressed with limit.
func DecodeEnvelope(data []byte, limit int64) (*Bundle, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedBundle, err)
	}
	if len(e.Manifest) == 0 || len(e.ModuleGz) == 0 {
		return nil, fmt.Errorf("%w: missing manifest or module", ErrMalformedBundle)
	}
	if limit <= 0 {
		limit = 64 << 20
	}
	mod, err := Gunzip(e.ModuleGz, limit)
	if err != nil {
		return nil, err
	}
	return &Bundle{Manifest: e.Manifest, Signature: e.Signature, KeyID: e.KeyID, Module: mod}, nil
}

// Bundle directory layout (also the ConfigMap keys used by the operator).
const (
	FileManifest  = "manifest.json"
	FileSignature = "manifest.sig" // base64 signature, then "\n", then key id
	FileModule    = "module.wasm.gz"
)

// WriteDir writes the bundle as three files into dir.
func (b *Bundle) WriteDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sig := fmt.Sprintf("%s\n%s\n", encodeB64(b.Signature), b.KeyID)
	for name, data := range map[string][]byte{
		FileManifest:  b.Manifest,
		FileSignature: []byte(sig),
		FileModule:    Gzip(b.Module),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ParseSignatureFile parses the manifest.sig format.
func ParseSignatureFile(data []byte) (sig []byte, keyID string, err error) {
	lines := strings.Fields(string(data))
	if len(lines) != 2 {
		return nil, "", fmt.Errorf("%w: signature file must contain signature and key id", ErrMalformedBundle)
	}
	sig, err = decodeB64(lines[0])
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrMalformedBundle, err)
	}
	return sig, lines[1], nil
}

// ReadDir reads a bundle written by WriteDir.
func ReadDir(dir string, limit int64) (*Bundle, error) {
	m, err := os.ReadFile(filepath.Join(dir, FileManifest))
	if err != nil {
		return nil, err
	}
	s, err := os.ReadFile(filepath.Join(dir, FileSignature))
	if err != nil {
		return nil, err
	}
	gz, err := os.ReadFile(filepath.Join(dir, FileModule))
	if err != nil {
		return nil, err
	}
	sig, kid, err := ParseSignatureFile(s)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 64 << 20
	}
	mod, err := Gunzip(gz, limit)
	if err != nil {
		return nil, err
	}
	return &Bundle{Manifest: m, Signature: sig, KeyID: kid, Module: mod}, nil
}

// ---------------------------------------------------------------------------
// Key files (PEM: PKCS#8 private, PKIX public).

// WritePrivateKey writes priv as PEM with mode 0600.
func WritePrivateKey(path string, priv ed25519.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

// WritePublicKey writes pub as PEM.
func WritePublicKey(path string, pub ed25519.PublicKey) error {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)
}

// ReadPrivateKey reads a PEM PKCS#8 ed25519 key.
func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key", path)
	}
	return priv, nil
}

// ParsePublicKey parses a PEM PKIX ed25519 public key.
func ParsePublicKey(b []byte) (ed25519.PublicKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM block")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an ed25519 public key")
	}
	return pub, nil
}

// LoadPublicKeys loads every *.pub / *.pem public key from the given files
// or directories.
func LoadPublicKeys(paths ...string) ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for _, p := range paths {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		files := []string{p}
		if st.IsDir() {
			files = nil
			ents, err := os.ReadDir(p)
			if err != nil {
				return nil, err
			}
			for _, e := range ents {
				n := e.Name()
				if !e.IsDir() && (strings.HasSuffix(n, ".pub") || strings.HasSuffix(n, ".pem")) && !strings.HasPrefix(n, "..") {
					files = append(files, filepath.Join(p, n))
				}
			}
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			k, err := ParsePublicKey(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			out = append(out, k)
		}
	}
	return out, nil
}
