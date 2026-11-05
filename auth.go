package providerkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// APIKeyAuth implements the SDK auth provider for API-key-only providers.
// Validate may validate additional provider-owned fields; its errors are redacted
// by Runtime unless explicitly represented as public pluginabi.Error values.
// It is stateless: duplicate-file/account policy belongs to the provider.
//
// There is no login flow: StartLogin and PollLogin always fail with
// api_key_only, so API-key providers show no login card in the management UI
// and auth files load through the host's generic file path. The kit's
// script-based API-key login page was removed; OpenRouter-style OAuth/PKCE
// logins live in the provider, not in the kit.
type APIKeyAuth struct {
	Provider string
	Validate func(map[string]any) error
}

func (p APIKeyAuth) Identifier() string { return p.Provider }

// credentialProviderSource is the optional extension the host probes (through
// the auth identifier response) to learn which additional credential types a
// plugin owns, for example a legacy provider key the plugin takes over.
type credentialProviderSource interface {
	CredentialProviders() []string
}

// authIdentifierResponse preserves optional credential ownership metadata for
// the host to interpret. New still rejects MultiAuthProvider registrations.
func authIdentifierResponse(p pluginapi.AuthProvider) map[string]any {
	resp := map[string]any{"identifier": p.Identifier()}
	if creds, ok := p.(credentialProviderSource); ok {
		resp["credential_providers"] = creds.CredentialProviders()
	}
	return resp
}

func object(raw []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, rpcError("invalid_auth", "auth storage must be a JSON object", 400)
	}
	out := make(map[string]any)
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, rpcError("invalid_auth", "invalid auth object", 400)
		}
		key, ok := token.(string)
		if !ok {
			return nil, rpcError("invalid_auth", "invalid auth field", 400)
		}
		if _, exists := out[key]; exists {
			return nil, rpcError("invalid_auth", "duplicate auth field", 400)
		}
		var value any
		if d.Decode(&value) != nil {
			return nil, rpcError("invalid_auth", "invalid auth field", 400)
		}
		out[key] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, rpcError("invalid_auth", "invalid auth object", 400)
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, rpcError("invalid_auth", "trailing auth data", 400)
	}
	return out, nil
}

// AuthFields applies the same merge precedence as persisted host auth storage:
// Metadata overrides StorageJSON. Callers own the returned map.
func AuthFields(storage []byte, metadata map[string]any) (map[string]any, error) {
	fields, err := object(storage)
	if err != nil {
		return nil, err
	}
	for k, v := range metadata {
		fields[k] = v
	}
	return fields, nil
}

// APIKey reads a nonempty api_key using host metadata precedence. It never
// includes the key or raw storage in an error.
func APIKey(storage []byte, metadata map[string]any) (string, error) {
	fields, err := AuthFields(storage, metadata)
	if err != nil {
		return "", err
	}
	key, ok := fields["api_key"].(string)
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return "", rpcError("invalid_auth", "a nonempty api_key is required", 400)
	}
	return key, nil
}

func (p APIKeyAuth) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	fields, err := object(req.RawJSON)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	kind, _ := fields["type"].(string)
	if kind != p.Provider || p.Provider == "" || req.Provider != "" && req.Provider != p.Provider {
		return pluginapi.AuthParseResponse{}, nil
	}
	key, ok := fields["api_key"].(string)
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return pluginapi.AuthParseResponse{}, rpcError("invalid_auth", "a nonempty api_key is required", 400)
	}
	fields["api_key"] = key
	for _, name := range []string{"id", "label", "prefix", "proxy_url"} {
		if value, exists := fields[name]; exists {
			if _, ok := value.(string); !ok {
				return pluginapi.AuthParseResponse{}, rpcError("invalid_auth", "host auth text fields must be strings", 400)
			}
		}
	}
	disabled := false
	if value, exists := fields["disabled"]; exists {
		var ok bool
		disabled, ok = value.(bool)
		if !ok {
			return pluginapi.AuthParseResponse{}, rpcError("invalid_auth", "disabled must be boolean", 400)
		}
	}
	attrs := map[string]string{}
	for _, name := range []string{"weight", "priority"} {
		if value, exists := fields[name]; exists {
			number, ok := value.(json.Number)
			if !ok {
				return pluginapi.AuthParseResponse{}, rpcError("invalid_auth", "weight and priority must be integers", 400)
			}
			n, err := number.Int64()
			if err != nil || name == "weight" && n < 0 {
				return pluginapi.AuthParseResponse{}, rpcError("invalid_auth", "invalid weight or priority", 400)
			}
			attrs[name] = strconv.FormatInt(n, 10)
		}
	}
	if p.Validate != nil {
		if err = p.Validate(fields); err != nil {
			return pluginapi.AuthParseResponse{}, err
		}
	}
	id, _ := fields["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		digest := sha256.Sum256([]byte(key))
		id = p.Provider + ":" + hex.EncodeToString(digest[:6])
	}
	// Normalize the prefix exactly like the host filestore: the plugin path
	// pins whatever ParseAuth returns verbatim, so a slashed prefix must be
	// rejected here rather than silently treated as unpinned downstream.
	// The normalized value is written back so SDK metadata agrees with Prefix.
	if value, exists := fields["prefix"]; exists {
		prefix, ok := value.(string)
		if !ok {
			return pluginapi.AuthParseResponse{}, rpcError("invalid_auth", "host auth text fields must be strings", 400)
		}
		normalized, err := NormalizePrefix(prefix)
		if err != nil {
			return pluginapi.AuthParseResponse{}, err
		}
		fields["prefix"] = normalized
	}
	text := func(name string) string { value, _ := fields[name].(string); return strings.TrimSpace(value) }
	auth := pluginapi.AuthData{Provider: p.Provider, ID: id, FileName: req.FileName, Label: text("label"), Prefix: text("prefix"), ProxyURL: text("proxy_url"), Disabled: disabled, Attributes: attrs, Metadata: map[string]any{"type": p.Provider}}
	for _, name := range []string{"id", "label", "prefix", "proxy_url", "disabled", "weight", "priority"} {
		if v, ok := fields[name]; ok {
			auth.Metadata[name] = v
			delete(fields, name)
		}
	}
	auth.StorageJSON, err = json.Marshal(fields)
	if err != nil {
		return pluginapi.AuthParseResponse{}, rpcError("invalid_auth", "cannot encode auth storage", 400)
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}
func (p APIKeyAuth) StartLogin(_ context.Context, _ pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, rpcError("api_key_only", "API-key only: create an auth JSON file", 400)
}
func (p APIKeyAuth) PollLogin(_ context.Context, _ pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{}, rpcError("api_key_only", "API-key only: create an auth JSON file", 400)
}
func (p APIKeyAuth) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	if _, err := APIKey(req.StorageJSON, req.Metadata); err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	// Preserve storage byte-for-byte; the host overlays metadata when persisting.
	text := func(name string) string { v, _ := req.Metadata[name].(string); return v }
	disabled, _ := req.Metadata["disabled"].(bool)
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{Provider: p.Provider, ID: req.AuthID, StorageJSON: bytes.Clone(req.StorageJSON), Metadata: req.Metadata, Attributes: req.Attributes, Label: text("label"), Prefix: text("prefix"), ProxyURL: text("proxy_url"), Disabled: disabled}}, nil
}
