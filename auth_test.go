package providerkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAuthParserStableIdentityAndHostFields(t *testing.T) {
	p := APIKeyAuth{Provider: "echo"}
	req := pluginapi.AuthParseRequest{FileName: "account.json", RawJSON: []byte(`{"type":"echo","api_key":"example-key","label":"Example","prefix":"echo","weight":2,"priority":3,"disabled":true,"region":"global"}`)}
	resp, err := p.ParseAuth(context.Background(), req)
	if err != nil || !resp.Handled {
		t.Fatalf("parse %+v %v", resp, err)
	}
	if resp.Auth.ID != "echo:c018c41c1afa" {
		t.Fatalf("identity actual: %s", resp.Auth.ID)
	}
	// The fingerprint must be key-derived, never file/path-derived.
	req.FileName = "renamed.json"
	other, err := p.ParseAuth(context.Background(), req)
	if err != nil || other.Auth.ID != resp.Auth.ID || !strings.HasPrefix(resp.Auth.ID, "echo:") || len(resp.Auth.ID) != 17 {
		t.Fatalf("identity changed: %+v %v", other, err)
	}
	if resp.Auth.Label != "Example" || !resp.Auth.Disabled || resp.Auth.Prefix != "echo" || resp.Auth.Attributes["weight"] != "2" || resp.Auth.Attributes["priority"] != "3" {
		t.Fatalf("host fields: %+v", resp.Auth)
	}
	var store map[string]any
	if err = json.Unmarshal(resp.Auth.StorageJSON, &store); err != nil {
		t.Fatal(err)
	}
	if store["api_key"] != "example-key" || store["region"] != "global" || store["label"] != nil || store["weight"] != nil {
		t.Fatalf("storage: %+v", store)
	}
	key, err := APIKey(resp.Auth.StorageJSON, map[string]any{"api_key": "replacement-key"})
	if err != nil || key != "replacement-key" {
		t.Fatalf("metadata precedence: %q %v", key, err)
	}
}
func TestAuthParserRejectsMalformedWithoutHalfAuth(t *testing.T) {
	p := APIKeyAuth{Provider: "echo"}
	for _, raw := range []string{`null`, `[]`, `{`, `{"type":"echo"}`, `{"type":"echo","api_key":12}`, `{"type":"echo","api_key":" "}`, `{"type":"echo","api_key":"example-key","weight":1.5}`, `{"type":"echo","api_key":"example-key","disabled":"false"}`, `{"type":"echo","api_key":"one","api_key":"two"}`, `{"type":"echo","api_key":"example-key","id":false}`,
		`{"type":"echo","api_key":"example-key","label":7}`, `{"type":"echo","api_key":"example-key","weight":-1}`, `{"type":"echo","api_key":"example-key","weight":"2"}`, `{"type":"echo","api_key":"example-key","priority":9223372036854775808}`,
		`{"type":"echo","api_key":"example-key","label":"a","label":"b"}`, `{"type":"echo","api_key":"example-key"} {}`, `{"type":"echo","api_key":"example-key"}]`} {
		t.Run(raw, func(t *testing.T) {
			resp, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(raw)})
			if err == nil || resp.Handled || resp.Auth.ID != "" || strings.Contains(err.Error(), "example-key") {
				t.Fatalf("accepted %s: %+v %v", raw, resp, err)
			}
		})
	}
	resp, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"other","api_key":"example-key"}`)})
	if err != nil || resp.Handled {
		t.Fatalf("wrong provider %+v %v", resp, err)
	}
}
func TestAuthExplicitIDAndRefresh(t *testing.T) {
	p := APIKeyAuth{Provider: "echo"}
	resp, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"echo","id":"custom","api_key":"example-key"}`)})
	if err != nil || resp.Auth.ID != "custom" {
		t.Fatal(resp, err)
	}
	ref, err := p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: resp.Auth.ID, StorageJSON: resp.Auth.StorageJSON, Metadata: resp.Auth.Metadata, Attributes: resp.Auth.Attributes})
	if err != nil || ref.Auth.ID != "custom" || string(ref.Auth.StorageJSON) != string(resp.Auth.StorageJSON) {
		t.Fatal(ref, err)
	}
	if _, err = p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{}); err == nil {
		t.Fatal("API key helper advertised login")
	}
}
func TestAuthParserProviderScopeAndNormalization(t *testing.T) {
	ctx := context.Background()
	raw := []byte(`{"type":"echo","api_key":"example-key"}`)
	// A file scoped to another provider, or a parser without a provider, must not be claimed.
	if resp, err := (APIKeyAuth{Provider: "echo"}).ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "other", RawJSON: raw}); err != nil || resp.Handled {
		t.Fatalf("claimed another provider's file: %+v %v", resp, err)
	}
	if resp, err := (APIKeyAuth{}).ParseAuth(ctx, pluginapi.AuthParseRequest{RawJSON: []byte(`{"api_key":"example-key"}`)}); err != nil || resp.Handled {
		t.Fatalf("unnamed parser claimed untyped file: %+v %v", resp, err)
	}
	p := APIKeyAuth{Provider: "echo"}
	if resp, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "echo", RawJSON: raw}); err != nil || !resp.Handled || resp.Auth.Provider != "echo" {
		t.Fatalf("scoped parse %+v %v", resp, err)
	}
	// Surrounding whitespace must neither reach upstream nor fork the account identity.
	resp, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"echo","api_key":"  example-key\n","id":"  ","priority":-5}`)})
	if err != nil || resp.Auth.ID != "echo:c018c41c1afa" || resp.Auth.Attributes["priority"] != "-5" {
		t.Fatalf("normalized parse %+v %v", resp, err)
	}
	if key, err := APIKey(resp.Auth.StorageJSON, nil); err != nil || key != "example-key" {
		t.Fatalf("stored key %q %v", key, err)
	}
	if !strings.Contains(string(resp.Auth.StorageJSON), `"api_key":"example-key"`) {
		t.Fatalf("storage kept untrimmed key: %s", resp.Auth.StorageJSON)
	}
	resp, err = p.ParseAuth(ctx, pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"echo","api_key":"example-key","id":" custom "}`)})
	if err != nil || resp.Auth.ID != "custom" {
		t.Fatalf("explicit id %+v %v", resp, err)
	}
}
func TestAuthParserPrefixNormalization(t *testing.T) {
	p := APIKeyAuth{Provider: "echo"}
	ctx := context.Background()
	// Surrounding slashes trim exactly like the host filestore, and the
	// normalized value reaches both the SDK prefix and the persisted metadata.
	resp, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"echo","api_key":"example-key","prefix":" /acct/ "}`)})
	if err != nil || !resp.Handled || resp.Auth.Prefix != "acct" || resp.Auth.Metadata["prefix"] != "acct" {
		t.Fatalf("prefix not normalized: %+v %v", resp, err)
	}
	// A lone "/" trims to the unpinned state.
	resp, err = p.ParseAuth(ctx, pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"echo","api_key":"example-key","prefix":"/"}`)})
	if err != nil || !resp.Handled || resp.Auth.Prefix != "" {
		t.Fatalf("slash-only prefix not unpinned: %+v %v", resp, err)
	}
	// An inner slash would pin verbatim on the plugin path and merge pinning
	// namespaces, so it is rejected rather than silently unpinned.
	for _, raw := range []string{
		`{"type":"echo","api_key":"example-key","prefix":"a/b"}`,
		`{"type":"echo","api_key":"example-key","prefix":"team/a/"}`,
	} {
		resp, err := p.ParseAuth(ctx, pluginapi.AuthParseRequest{RawJSON: []byte(raw)})
		if e := MapError(err); e == nil || e.Code != "invalid_auth" || resp.Handled {
			t.Fatalf("slash prefix accepted %s: %+v %+v", raw, resp, e)
		}
	}
}
func TestAuthParserValidateHook(t *testing.T) {
	var seen map[string]any
	p := APIKeyAuth{Provider: "echo", Validate: func(fields map[string]any) error {
		seen = fields
		if fields["region"] == nil {
			return &pluginabi.Error{Code: "region_required", Message: "region is required", HTTPStatus: 400}
		}
		return nil
	}}
	resp, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"echo","api_key":" example-key "}`)})
	if e := MapError(err); e == nil || e.Code != "region_required" || resp.Handled || resp.Auth.ID != "" {
		t.Fatalf("validation ignored: %+v %+v", resp, e)
	}
	if seen["api_key"] != "example-key" {
		t.Fatalf("Validate saw untrimmed key %q", seen["api_key"])
	}
	seen = nil
	if resp, err = p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"other","api_key":"example-key"}`)}); err != nil || resp.Handled || seen != nil {
		t.Fatalf("validated another provider's file: %+v %v", resp, err)
	}
	if resp, err = p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"echo","api_key":"example-key","region":"eu"}`)}); err != nil || !resp.Handled {
		t.Fatalf("valid file rejected: %+v %v", resp, err)
	}
}
func TestAuthValidateErrorRedactedThroughRuntime(t *testing.T) {
	p := APIKeyAuth{Provider: "echo", Validate: func(fields map[string]any) error {
		return fmt.Errorf("upstream rejected %v", fields["api_key"])
	}}
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{AuthProvider: p}}, nil)
	register(t, r)
	env := rpc(t, r, "auth.parse", pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"echo","api_key":"example-key"}`)})
	if env.OK || env.Error == nil || strings.Contains(env.Error.Message, "example-key") || env.Error.Code != "plugin_error" {
		t.Fatalf("validation error leaked or accepted: %+v", env)
	}
}
func TestAPIKeyPrecedenceAndRefreshFailures(t *testing.T) {
	p := APIKeyAuth{Provider: "echo"}
	storage := []byte(`{"api_key":"example-key"}`)
	for name, tc := range map[string]struct {
		storage  []byte
		metadata map[string]any
	}{
		"blank metadata overrides storage": {storage, map[string]any{"api_key": " "}},
		"non-string metadata":              {storage, map[string]any{"api_key": 7}},
		"missing":                          {[]byte(`{"region":"eu"}`), nil},
		"storage not an object":            {[]byte(`"example-key"`), nil},
		"duplicate storage key":            {[]byte(`{"api_key":"example-key","api_key":"other-key"}`), nil},
	} {
		t.Run(name, func(t *testing.T) {
			if key, err := APIKey(tc.storage, tc.metadata); err == nil || key != "" || strings.Contains(err.Error(), "example-key") {
				t.Fatalf("APIKey %q %v", key, err)
			}
			if resp, err := p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: "a", StorageJSON: tc.storage, Metadata: tc.metadata}); err == nil || resp.Auth.ID != "" {
				t.Fatalf("refreshed unusable auth: %+v %v", resp, err)
			}
		})
	}
	if key, err := APIKey([]byte(`{"api_key":"  example-key "}`), nil); err != nil || key != "example-key" {
		t.Fatalf("trim %q %v", key, err)
	}
}
func TestAuthRefreshKeepsHostStateAndIsolatesStorage(t *testing.T) {
	storage := []byte(`{"api_key":"example-key"}`)
	req := pluginapi.AuthRefreshRequest{AuthID: "a", StorageJSON: storage, Attributes: map[string]string{"priority": "3"},
		Metadata: map[string]any{"label": "Example", "prefix": "echo", "proxy_url": "http://proxy.example", "disabled": true}}
	resp, err := (APIKeyAuth{Provider: "echo"}).RefreshAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	a := resp.Auth
	// Dropping Disabled on refresh would silently re-enable an account the operator turned off.
	if a.Provider != "echo" || a.ID != "a" || !a.Disabled || a.Label != "Example" || a.Prefix != "echo" || a.ProxyURL != "http://proxy.example" || a.Attributes["priority"] != "3" {
		t.Fatalf("refresh lost host state: %+v", a)
	}
	storage[2] = 'X'
	if string(a.StorageJSON) != `{"api_key":"example-key"}` {
		t.Fatalf("refreshed storage aliases the request: %s", a.StorageJSON)
	}
}
