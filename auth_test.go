package providerkit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	for _, raw := range []string{`null`, `[]`, `{`, `{"type":"echo"}`, `{"type":"echo","api_key":12}`, `{"type":"echo","api_key":" "}`, `{"type":"echo","api_key":"example-key","weight":1.5}`, `{"type":"echo","api_key":"example-key","disabled":"false"}`, `{"type":"echo","api_key":"one","api_key":"two"}`, `{"type":"echo","api_key":"example-key","id":false}`} {
		t.Run(raw, func(t *testing.T) {
			resp, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(raw)})
			if err == nil || resp.Auth.ID != "" {
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
