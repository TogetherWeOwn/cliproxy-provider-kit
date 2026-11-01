package providerkit

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"
)

func ownershipGuard(now *time.Time) *KeyOwnership {
	return &KeyOwnership{now: func() time.Time { return *now }}
}

func digestOf(key string) [32]byte { return sha256.Sum256([]byte(key)) }

func TestKeyOwnershipCommittedClaims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := ownershipGuard(&now)
	ctx := context.Background()
	digest := digestOf("example-key")

	if err := g.Claim(ctx, "a.json", digest); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// A watcher's repeated parse of the same file is safe.
	if err := g.Claim(ctx, "a.json", digest); err != nil {
		t.Fatalf("same-file reparse: %v", err)
	}
	if err := g.Claim(ctx, "b.json", digest); err == nil || MapError(err).Code != "duplicate_auth" {
		t.Fatalf("other file with same key = %v, want duplicate_auth", err)
	}
	// Reparsing a file with a new key frees its old digest.
	other := digestOf("other-key")
	if err := g.Claim(ctx, "a.json", other); err != nil {
		t.Fatalf("file changes key: %v", err)
	}
	if err := g.Claim(ctx, "b.json", digest); err != nil {
		t.Fatalf("freed digest: %v", err)
	}
}

// TestKeyOwnershipProvisionalExpires is the abandoned-login repro from
// issue #13: a login-validation parse must not hold a key past the login
// session, so uploading the same key later under a different filename
// succeeds once the session has expired.
func TestKeyOwnershipProvisionalExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := ownershipGuard(&now)
	digest := digestOf("example-key")
	loginCtx := withProvisionalParse(context.Background(), now.Add(15*time.Minute))

	if err := g.Claim(loginCtx, "echo-login.json", digest); err != nil {
		t.Fatalf("provisional claim: %v", err)
	}
	// While the login is in flight another filename is still a duplicate,
	// and the login filename itself may revalidate.
	if err := g.Claim(context.Background(), "upload.json", digest); err == nil {
		t.Fatal("upload during pending login accepted")
	}
	if err := g.Claim(loginCtx, "echo-login.json", digest); err != nil {
		t.Fatalf("provisional revalidate: %v", err)
	}
	// After the session expires the reservation is gone: the upload the
	// stale committed claim used to reject now takes ownership.
	now = now.Add(16 * time.Minute)
	if err := g.Claim(context.Background(), "upload.json", digest); err != nil {
		t.Fatalf("upload after session expiry: %v", err)
	}
	if err := g.Claim(context.Background(), "echo-login.json", digest); err == nil {
		t.Fatal("login filename accepted after upload took ownership")
	}
}

func TestKeyOwnershipProvisionalRespectsCommittedOwner(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := ownershipGuard(&now)
	digest := digestOf("example-key")
	if err := g.Claim(context.Background(), "real.json", digest); err != nil {
		t.Fatal(err)
	}
	// Submit-time feedback is preserved: a login for a key a real file
	// already owns is rejected as a duplicate.
	loginCtx := withProvisionalParse(context.Background(), now.Add(time.Minute))
	if err := g.Claim(loginCtx, "echo-login.json", digest); err == nil || MapError(err).Code != "duplicate_auth" {
		t.Fatalf("provisional vs committed owner = %v, want duplicate_auth", err)
	}
	// The owning file itself may still pass a provisional parse (re-login
	// with the key already saved under the login filename).
	if err := g.Claim(loginCtx, "real.json", digest); err != nil {
		t.Fatalf("provisional by owner: %v", err)
	}
}

func TestKeyOwnershipCommitPromotesPending(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := ownershipGuard(&now)
	digest := digestOf("example-key")
	loginCtx := withProvisionalParse(context.Background(), now.Add(15*time.Minute))
	if err := g.Claim(loginCtx, "echo-login.json", digest); err != nil {
		t.Fatal(err)
	}
	// The host saved the login and parsed the real file: ownership commits
	// and no longer expires with the session.
	if err := g.Claim(context.Background(), "echo-login.json", digest); err != nil {
		t.Fatalf("commit after login: %v", err)
	}
	now = now.Add(time.Hour)
	if err := g.Claim(context.Background(), "upload.json", digest); err == nil {
		t.Fatal("committed ownership expired with the login session")
	}
}

func TestProvisionalParseExpiry(t *testing.T) {
	if _, ok := ProvisionalParseExpiry(context.Background()); ok {
		t.Fatal("plain context reported provisional")
	}
	if _, ok := ProvisionalParseExpiry(nil); ok { //nolint:staticcheck // nil context is the case under test
		t.Fatal("nil context reported provisional")
	}
	expiry := time.Unix(1_700_000_000, 0)
	got, ok := ProvisionalParseExpiry(withProvisionalParse(context.Background(), expiry))
	if !ok || !got.Equal(expiry) {
		t.Fatalf("provisional context = %v, %v; want %v, true", got, ok, expiry)
	}
}
