package providerkit

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPrefixClaimsRejectSharedPrefix(t *testing.T) {
	var claims PrefixClaims
	if err := claims.Claim("a.json", "acct-a", []byte("key-one")); err != nil {
		t.Fatal(err)
	}
	// Same file, same key, same prefix reparses cleanly.
	if err := claims.Claim("a.json", "acct-a", []byte("key-one")); err != nil {
		t.Fatal(err)
	}
	// A second credential under the same prefix must fail loudly, not merge.
	err := claims.Claim("b.json", "acct-a", []byte("key-two"))
	if MapError(err).Code != "duplicate_prefix" {
		t.Fatalf("shared prefix accepted: %v", err)
	}
	// The rejected claim leaves no residue: the first file still owns it.
	if err := claims.Claim("c.json", "acct-a", []byte("key-three")); MapError(err).Code != "duplicate_prefix" {
		t.Fatalf("rejected claim evicted the owner: %v", err)
	}
}

func TestPrefixClaimsDuplicateKeyAcrossFiles(t *testing.T) {
	var claims PrefixClaims
	if err := claims.Claim("a.json", "acct-a", []byte("same-key")); err != nil {
		t.Fatal(err)
	}
	if err := claims.Claim("b.json", "acct-b", []byte("same-key")); MapError(err).Code != "duplicate_auth" {
		t.Fatalf("duplicate key accepted: %v", err)
	}
}

func TestPrefixClaimsRotationAndRelease(t *testing.T) {
	var claims PrefixClaims
	if err := claims.Claim("a.json", "acct-a", []byte("old-key")); err != nil {
		t.Fatal(err)
	}
	// Rotating the key in the same file releases the old digest.
	if err := claims.Claim("a.json", "acct-a", []byte("new-key")); err != nil {
		t.Fatal(err)
	}
	// The old key is now free to appear elsewhere (moved file).
	if err := claims.Claim("b.json", "acct-b", []byte("old-key")); err != nil {
		t.Fatal(err)
	}
	// Moving a file off a prefix frees the prefix for a new credential.
	if err := claims.Claim("a.json", "", []byte("new-key")); err != nil {
		t.Fatal(err)
	}
	if err := claims.Claim("c.json", "acct-a", []byte("third-key")); err != nil {
		t.Fatalf("released prefix not reusable: %v", err)
	}
	claims.Release("c.json")
	if err := claims.Claim("d.json", "acct-a", []byte("fourth-key")); err != nil {
		t.Fatalf("release did not free the prefix: %v", err)
	}
}

func TestPrefixClaimsUnpinnedSharing(t *testing.T) {
	var claims PrefixClaims
	// Empty prefixes are the normal unpinned state: any number of files may
	// share them.
	for _, file := range []string{"a.json", "b.json", "c.json"} {
		if err := claims.Claim(file, "", []byte("key-"+file)); err != nil {
			t.Fatal(err)
		}
	}
	// Leading/trailing slashes normalize like the filestore: "acct/" claims "acct".
	if err := claims.Claim("d.json", "acct/", []byte("key-d")); err != nil {
		t.Fatal(err)
	}
	if err := claims.Claim("e.json", "acct", []byte("key-e")); MapError(err).Code != "duplicate_prefix" {
		t.Fatalf("normalized prefix collision accepted: %v", err)
	}
	// An inner slash can never be unpinned: the host pins it verbatim on the
	// plugin path, so claiming it must fail loudly instead of silently sharing
	// the unpinned namespace.
	if err := claims.Claim("f.json", "has/slash", []byte("key-f")); MapError(err).Code != "invalid_auth" {
		t.Fatalf("slash prefix accepted: %v", err)
	}
}

func TestQuotaObservationBoundToCredential(t *testing.T) {
	var calls int
	q := NewCachedQuota("echo", "Echo", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		calls++
		return quotaFixture(.5), nil
	})
	oldReq := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo", StorageJSON: []byte(`{"api_key":"old-key"}`)}
	newReq := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo", StorageJSON: []byte(`{"api_key":"new-key"}`)}
	// A credential-bound observation serves its own generation …
	if err := q.Store(oldReq, quotaFixture(.9)); err != nil {
		t.Fatal(err)
	}
	if resp, err := q.FetchQuota(context.Background(), oldReq); err != nil || resp.Groups[0].Buckets[0].RemainingFraction != .9 || calls != 0 {
		t.Fatalf("bound observation missed: %+v calls %d %v", resp, calls, err)
	}
	// … but never a rotated credential under the same identity: poll fresh.
	if resp, err := q.FetchQuota(context.Background(), newReq); err != nil || resp.Groups[0].Buckets[0].RemainingFraction != .5 || calls != 1 {
		t.Fatalf("stale generation served: %+v calls %d %v", resp, calls, err)
	}
	// Unbound (header-only, unknown credential) observations keep the fast path.
	q2 := NewCachedQuota("echo", "Echo", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		calls++
		return quotaFixture(.5), nil
	})
	if err := q2.Store(pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "echo"}, quotaFixture(.9)); err != nil {
		t.Fatal(err)
	}
	if resp, err := q2.FetchQuota(context.Background(), newReq); err != nil || resp.Groups[0].Buckets[0].RemainingFraction != .9 {
		t.Fatalf("unbound observation not served: %+v %v", resp, err)
	}
}
