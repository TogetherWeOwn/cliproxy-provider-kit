package providerkit

import (
	"crypto/sha256"
	"strings"
	"sync"
)

// PrefixClaims tracks single-segment host prefixes claimed by auth files so
// two credentials cannot silently share one pinning namespace (G-K1). The
// host strips a credential's prefix before upstream dispatch, so two accounts
// sharing a prefix merge into one namespace and defeat per-account pinning.
// APIKeyAuth stays stateless by design; providers own this optional tracker
// and the lifecycle that applies or removes their auths. Claim is an immediate
// update, not a reservation tied to a host parse/install transaction. Providers
// must serialize claims with committed auth changes; speculative ParseAuth
// calls alone cannot guarantee uniqueness among installed host auths.
//
// Only non-empty, slash-free prefixes are claimed. The host pins the returned
// prefix verbatim (TrimSpace only). Use APIKeyAuth's parsed Prefix, or call
// NormalizePrefix and use its result both for Claim and the returned auth.
// Claim normalizes for comparison but cannot rewrite a caller's auth value.
// Empty prefixes are the normal unpinned state shared by any number of files.
// Credential identity is the SHA-256 of the provider's credential bytes (for
// example the trimmed API key); only digests are retained, never key material.
type PrefixClaims struct {
	mu       sync.Mutex
	byPrefix map[string]prefixClaim
	byFile   map[string]fileClaim
}

type prefixClaim struct {
	file   string
	digest [32]byte
}

type fileClaim struct {
	prefix string
	digest [32]byte
}

// NormalizePrefix applies the host filestore's prefix contract to a
// plugin-returned prefix: surrounding whitespace and "/" are trimmed, and an
// inner "/" is rejected with invalid_auth. The plugin path pins whatever the
// plugin returns verbatim, so silently mapping a slashed prefix to unpinned
// would leave two files sharing one host namespace while the tracker believes
// both are unpinned. The programme rule is never to silently drop: reject.
func NormalizePrefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(strings.Trim(strings.TrimSpace(prefix), "/"))
	if strings.Contains(prefix, "/") {
		return "", rpcError("invalid_auth", "prefix must not contain \"/\"", 400)
	}
	return prefix, nil
}

// Claim records file's use of prefix for the given credential. It returns a
// duplicate_prefix *pluginabi.Error (via rpcError, HTTP 400) naming both files
// when a different credential already claims prefix, and duplicate_auth when
// the same credential is already configured in another file. Reparsing the
// same file, changing a file's key, or moving a file between prefixes updates
// the tracker instead of erroring. A prefix containing "/" is rejected with
// invalid_auth: the host pins it verbatim on the plugin path, so it can never
// be treated as unpinned.
func (t *PrefixClaims) Claim(file, prefix string, credential []byte) error {
	var err error
	if prefix, err = NormalizePrefix(prefix); err != nil {
		return err
	}
	digest := sha256.Sum256(credential)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byPrefix == nil {
		t.byPrefix = make(map[string]prefixClaim)
		t.byFile = make(map[string]fileClaim)
	}
	// Validate before mutating so a rejected claim leaves no residue.
	if prefix != "" {
		if claim, ok := t.byPrefix[prefix]; ok && claim.file != file && claim.digest != digest {
			return rpcError("duplicate_prefix", "the prefix "+quotePrefix(prefix)+" is already claimed by auth file "+quotePrefix(claim.file), 400)
		}
	}
	for other, claim := range t.byFile {
		if other != file && claim.digest == digest {
			return rpcError("duplicate_auth", "the same API key is already configured in auth file "+quotePrefix(other), 400)
		}
	}
	if old, ok := t.byFile[file]; ok && old != (fileClaim{prefix, digest}) {
		if old.prefix != "" {
			if claim, ok := t.byPrefix[old.prefix]; ok && claim.file == file {
				delete(t.byPrefix, old.prefix)
			}
		}
	}
	if prefix != "" {
		t.byPrefix[prefix] = prefixClaim{file, digest}
	}
	t.byFile[file] = fileClaim{prefix, digest}
	return nil
}

// Release forgets every claim held by file. Providers call it after an auth
// has been removed from active use, not merely when a speculative parse fails,
// so a removed auth cannot squat on a prefix or mask a genuine duplicate.
func (t *PrefixClaims) Release(file string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	claim, ok := t.byFile[file]
	if !ok {
		return
	}
	delete(t.byFile, file)
	if claim.prefix != "" {
		if current, ok := t.byPrefix[claim.prefix]; ok && current.file == file {
			delete(t.byPrefix, claim.prefix)
		}
	}
}

// quotePrefix renders a file or prefix name for error text. Names originate
// from host file paths and operator config, not from secrets; truncation
// bounds pathological lengths out of management error payloads.
func quotePrefix(name string) string {
	const maxPrefixName = 128
	name = strings.TrimSpace(name)
	if len(name) > maxPrefixName {
		name = name[:maxPrefixName] + "…"
	}
	if name == "" {
		return "(unnamed)"
	}
	return "\"" + name + "\""
}
