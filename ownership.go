package providerkit

import (
	"context"
	"sync"
	"time"
)

// Provisional (login-validation) parses.
//
// APIKeyLogin validates each submitted key through the provider's full
// ParseAuth pipeline before the login is polled or the host saves an auth
// file. A provider that records filename ownership of a key during any parse
// would keep that claim even when the operator abandons the login, so a
// later upload of the same key under a different filename fails as a
// duplicate even though no login file exists.
//
// The login submit therefore marks its validation parse as provisional: the
// context handed to APIKeyLoginConfig.Parse carries the login session's
// expiry. Ownership guards must treat a provisional parse as a check, not a
// commit. Providers using KeyOwnership get that behavior automatically;
// a hand-rolled guard should consult ProvisionalParseExpiry and, for a
// provisional parse, reject duplicates owned by another filename without
// recording ownership of its own (or record it only until the expiry).

type provisionalParseKey struct{}

func withProvisionalParse(ctx context.Context, expiresAt time.Time) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, provisionalParseKey{}, expiresAt)
}

// ProvisionalParseExpiry reports whether ctx belongs to a login-validation
// parse (see APIKeyLogin) and, if so, when the login session behind it
// expires. Parses the host initiates for real auth files are never
// provisional.
func ProvisionalParseExpiry(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	expiresAt, ok := ctx.Value(provisionalParseKey{}).(time.Time)
	return expiresAt, ok
}

// KeyOwnership is the shared filename-ownership duplicate guard for
// ParseAuth pipelines: one key digest may be configured in only one auth
// filename at a time, so a duplicate file is rejected while a watcher's
// repeated parse of the same file is safe. Only SHA-256 digests and
// filenames are retained, never keys.
//
// A committed (host-initiated, non-provisional) parse records ownership
// until the same filename is parsed with a different digest or the plugin
// restarts; deleting or renaming an auth file therefore still needs a
// plugin restart to free its key, as with the hand-rolled guards this
// replaces. A provisional (login-validation) parse instead reserves the
// digest only until the login session expires: it rejects a key another
// filename already owns, and blocks other filenames while the login is in
// flight, but the reservation is dropped lazily once expired. An abandoned
// login, an evicted session, or a login the host fails to persist can thus
// hold a key for at most the login TTL, never until restart. Ownership of a
// completed login commits when the host parses the auth file it saved.
//
// The zero value is ready to use. KeyOwnership is safe for concurrent use.
type KeyOwnership struct {
	mu      sync.Mutex
	files   map[string][32]byte
	owners  map[[32]byte]string
	pending map[[32]byte]pendingClaim
	now     func() time.Time
}

type pendingClaim struct {
	file      string
	expiresAt time.Time
}

func (g *KeyOwnership) nowLocked() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func duplicateAuthError() error {
	return rpcError("duplicate_auth", "the same API key is already configured in another auth file", 400)
}

// Claim checks filename's ownership of digest and records it. For a
// provisional parse (ProvisionalParseExpiry reports true for ctx) the claim
// is only reserved until the login session expires; any other parse commits
// ownership. Claim returns a duplicate_auth error when another filename
// owns or has reserved the digest.
func (g *KeyOwnership) Claim(ctx context.Context, fileName string, digest [32]byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.files == nil {
		g.files = make(map[string][32]byte)
		g.owners = make(map[[32]byte]string)
		g.pending = make(map[[32]byte]pendingClaim)
	}
	now := g.nowLocked()
	for d, claim := range g.pending {
		if !now.Before(claim.expiresAt) {
			delete(g.pending, d)
		}
	}
	if file, ok := g.owners[digest]; ok && file != fileName {
		return duplicateAuthError()
	}
	if claim, ok := g.pending[digest]; ok && claim.file != fileName {
		return duplicateAuthError()
	}
	if expiresAt, provisional := ProvisionalParseExpiry(ctx); provisional {
		// Overlapping login sessions for one key share the deterministic
		// login filename, so a resubmission from an earlier session must
		// never shorten the reservation a later session still relies on:
		// keep the latest expiry.
		if claim, ok := g.pending[digest]; g.owners[digest] != fileName && (!ok || expiresAt.After(claim.expiresAt)) {
			g.pending[digest] = pendingClaim{file: fileName, expiresAt: expiresAt}
		}
		return nil
	}
	if old, ok := g.files[fileName]; ok && old != digest {
		delete(g.owners, old)
	}
	g.files[fileName] = digest
	g.owners[digest] = fileName
	delete(g.pending, digest)
	return nil
}
