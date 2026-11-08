package delivery

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	Serial       uint64        `json:"serial"`
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
//
// Replay / downgrade policy. The loader keeps a high-water mark (HWM): the
// highest manifest serial that was applied (Commit), with that manifest's
// plugin hash. A manifest is accepted only if
//
//	serial > HWM                          (a newer patch), or
//	serial == HWM and sha256 == HWM hash  (re-applying the same patch, e.g.
//	                                       upgrade -> local rollback -> upgrade)
//
// and it has not expired. Rolling back is a local operation
// (Registry.Revert, /admin/rollback), never "apply an older signed patch".
// The HWM is in memory unless StateFile is set, in which case it is loaded
// at first use and rewritten atomically on every Commit, so it also survives
// restarts. Without StateFile a restarted process accepts any unexpired
// manifest again (the expiry bounds that window).
type Loader struct {
	Reg     *patchable.Registry
	Trusted []ed25519.PublicKey
	// WorkDir receives private copies of plugins (default: a fresh temp dir).
	WorkDir string
	// StateFile persists the replay high-water mark ("" = in memory only).
	StateFile string
	// Now is the clock for expiry checks (default time.Now).
	Now func() time.Time

	mu        sync.Mutex
	opened    map[string]*plugin.Plugin // by sha256: a plugin can be opened once per process
	hwm       hwmState
	hwmLoaded bool
}

type hwmState struct {
	Serial uint64 `json:"serial"`
	SHA256 string `json:"sha256"`
}

func (l *Loader) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// loadHWMLocked reads StateFile once. A missing file means "nothing applied
// yet"; an unreadable or corrupt one fails closed.
func (l *Loader) loadHWMLocked() error {
	if l.hwmLoaded {
		return nil
	}
	if l.StateFile != "" {
		b, err := os.ReadFile(l.StateFile)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return fmt.Errorf("delivery: read state %s: %w", l.StateFile, err)
		default:
			if err := json.Unmarshal(b, &l.hwm); err != nil {
				return fmt.Errorf("delivery: corrupt state %s: %w", l.StateFile, err)
			}
		}
	}
	l.hwmLoaded = true
	return nil
}

// checkFresh applies the expiry and replay policy to a verified manifest.
func (l *Loader) checkFresh(m Manifest) error {
	if now := l.now().Unix(); now >= m.ExpiresUnix {
		return fmt.Errorf("%w: expired at %s", ErrExpired, time.Unix(m.ExpiresUnix, 0).UTC().Format(time.RFC3339))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.loadHWMLocked(); err != nil {
		return err
	}
	switch {
	case m.Serial > l.hwm.Serial:
	case m.Serial == l.hwm.Serial && m.SHA256 == l.hwm.SHA256:
	default:
		return fmt.Errorf("%w: serial %d, high-water mark %d", ErrReplay, m.Serial, l.hwm.Serial)
	}
	return nil
}

// HighWaterMark returns the highest committed serial.
func (l *Loader) HighWaterMark() (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.loadHWMLocked(); err != nil {
		return 0, err
	}
	return l.hwm.Serial, nil
}

// Commit records that the patch described by rep (from a successful Load)
// was applied, raising the high-water mark. It is idempotent for the same
// patch and refuses to lower the mark.
func (l *Loader) Commit(rep Report) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.loadHWMLocked(); err != nil {
		return err
	}
	if rep.Serial < l.hwm.Serial || (rep.Serial == l.hwm.Serial && rep.SHA256 != l.hwm.SHA256) {
		return fmt.Errorf("%w: commit of serial %d below high-water mark %d", ErrReplay, rep.Serial, l.hwm.Serial)
	}
	next := hwmState{Serial: rep.Serial, SHA256: rep.SHA256}
	if l.StateFile != "" && next != l.hwm {
		b, _ := json.Marshal(next)
		if err := os.MkdirAll(filepath.Dir(l.StateFile), 0o700); err != nil {
			return err
		}
		tmp := l.StateFile + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, l.StateFile); err != nil {
			return err
		}
	}
	l.hwm = next
	return nil
}

// Load verifies and opens patch `name` from dir and returns the label and the
// validated replacement map, ready for Registry.Apply. Nothing is applied.
func (l *Loader) Load(dir, name string) (string, map[string]any, Report, error) {
	var rep Report
	m, err := ReadManifest(filepath.Join(dir, name+".manifest.json"))
	if err != nil {
		return "", nil, rep, err
	}
	rep.Label, rep.Serial = m.Label, m.Serial

	// 1. signature, then freshness (expiry, replay/downgrade) of the signed fields
	t := time.Now()
	if err := m.Verify(l.Trusted); err != nil {
		return "", nil, rep, err
	}
	if err := l.checkFresh(m); err != nil {
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
	if err != nil || cached {
		// The private copy is not mapped (open failed, or the plugin with
		// this hash was already mapped from an earlier copy): delete it, or
		// every upgrade -> rollback -> upgrade cycle leaks one .so copy.
		os.Remove(priv)
	}
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
		os.Remove(out.Name())
		return "", "", err
	}
	if err := os.Chmod(out.Name(), 0o500); err != nil {
		os.Remove(out.Name())
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
