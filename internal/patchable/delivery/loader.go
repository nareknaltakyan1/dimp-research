package delivery

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"plugin"
	"sort"
	"sync"
	"time"

	"github.com/nareknaltakyan1/dimp-research/internal/patchable"
)

// Report breaks down the cost of a load.
type Report struct {
	Label        string        `json:"label"`
	SHA256       string        `json:"sha256"`
	VerifySig    time.Duration `json:"verify_sig_ns"`
	PreCheck     time.Duration `json:"precheck_ns"`
	CopyHash     time.Duration `json:"copy_hash_ns"`
	Open         time.Duration `json:"open_ns"`
	PostCheck    time.Duration `json:"postcheck_ns"`
	CachedPlugin bool          `json:"cached_plugin"`
	Symbols      []string      `json:"symbols"`
}

// Loader loads signed patches into a registry.
type Loader struct {
	Reg     *patchable.Registry
	Trusted []ed25519.PublicKey
	// WorkDir receives private copies of plugins (default: a fresh temp dir).
	WorkDir string

	mu     sync.Mutex
	opened map[string]*plugin.Plugin // by sha256: a plugin can be opened once per process
}

// Load verifies and opens patch `name` from dir and returns the label and the
// validated replacement map, ready for Registry.Apply. Nothing is applied.
func (l *Loader) Load(dir, name string) (string, map[string]any, Report, error) {
	var rep Report
	m, err := ReadManifest(filepath.Join(dir, name+".manifest.json"))
	if err != nil {
		return "", nil, rep, err
	}
	rep.Label = m.Label

	// 1. signature
	t := time.Now()
	if err := m.Verify(l.Trusted); err != nil {
		return "", nil, rep, err
	}
	rep.VerifySig = time.Since(t)

	// 2. static fingerprint pre-check (before dlopen, which is irreversible)
	t = time.Now()
	for _, sym := range sortedSyms(m.Symbols) {
		want := m.Symbols[sym]
		s, ok := l.Reg.Lookup(sym)
		if !ok {
			return "", nil, rep, fmt.Errorf("pre-load check: %s: %w", sym, patchable.ErrUnknownSymbol)
		}
		if s.Fingerprint != want.Fingerprint {
			return "", nil, rep, fmt.Errorf("pre-load check: %s: %w: running binary has %s %q, patch was built for %s %q",
				sym, patchable.ErrSignature, s.Fingerprint, s.Signature, want.Fingerprint, want.Signature)
		}
	}
	rep.PreCheck = time.Since(t)

	// 3. private copy + hash
	t = time.Now()
	priv, sum, err := l.copyHashed(filepath.Join(dir, filepath.Base(m.Plugin)))
	if err != nil {
		return "", nil, rep, err
	}
	rep.SHA256 = sum
	if sum != m.SHA256 {
		os.Remove(priv)
		return "", nil, rep, fmt.Errorf("%w: file %s, manifest %s", ErrHash, sum, m.SHA256)
	}
	rep.CopyHash = time.Since(t)

	// 4. open + post-check
	t = time.Now()
	p, cached, err := l.open(sum, priv)
	if err != nil {
		return "", nil, rep, fmt.Errorf("plugin.Open: %w", err)
	}
	rep.CachedPlugin = cached
	rep.Open = time.Since(t)

	t = time.Now()
	sym, err := p.Lookup("Patches")
	if err != nil {
		return "", nil, rep, err
	}
	pp, ok := sym.(*map[string]any)
	if !ok {
		return "", nil, rep, fmt.Errorf("%w: Patches is %T, want map[string]any", ErrFormat, sym)
	}
	patches := *pp
	if len(patches) != len(m.Symbols) {
		return "", nil, rep, fmt.Errorf("%w: plugin exports %d patches, manifest lists %d", ErrFormat, len(patches), len(m.Symbols))
	}
	for symName, impl := range patches {
		want, ok := m.Symbols[symName]
		if !ok {
			return "", nil, rep, fmt.Errorf("%w: plugin patches %s, not in manifest", ErrFormat, symName)
		}
		_, fp, err := patchable.FingerprintOf(impl)
		if err != nil {
			return "", nil, rep, fmt.Errorf("%s: %w", symName, err)
		}
		if fp != want.Fingerprint {
			return "", nil, rep, fmt.Errorf("post-load check: %s: %w: manifest %s, plugin value %s", symName, patchable.ErrSignature, want.Fingerprint, fp)
		}
	}
	rep.PostCheck = time.Since(t)
	rep.Symbols = sortedSyms(m.Symbols)
	// Copy the map so later mutation of the plugin's variable cannot alter
	// what was checked.
	out := make(map[string]any, len(patches))
	for k, v := range patches {
		out[k] = v
	}
	return m.Label, out, rep, nil
}

func (l *Loader) open(sum, path string) (*plugin.Plugin, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.opened == nil {
		l.opened = map[string]*plugin.Plugin{}
	}
	if p, ok := l.opened[sum]; ok {
		// The Go runtime refuses to load the same plugin package twice
		// ("plugin already loaded"), so an upgrade -> rollback -> upgrade
		// cycle must reuse the already-mapped plugin.
		return p, true, nil
	}
	p, err := plugin.Open(path)
	if err != nil {
		return nil, false, err
	}
	l.opened[sum] = p
	return p, false, nil
}

func (l *Loader) copyHashed(src string) (string, string, error) {
	l.mu.Lock()
	if l.WorkDir == "" {
		d, err := os.MkdirTemp("", "dimp-hotpatch-")
		if err != nil {
			l.mu.Unlock()
			return "", "", err
		}
		l.WorkDir = d
	}
	wd := l.WorkDir
	l.mu.Unlock()
	in, err := os.Open(src)
	if err != nil {
		return "", "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(wd, "patch-*.so")
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		out.Close()
		os.Remove(out.Name())
		return "", "", err
	}
	if err := out.Close(); err != nil {
		return "", "", err
	}
	if err := os.Chmod(out.Name(), 0o500); err != nil {
		return "", "", err
	}
	return out.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// HashFile returns the hex sha256 of a file.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sortedSyms(m map[string]Symbol) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
