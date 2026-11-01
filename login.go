package providerkit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// API-key login over the plugin ManagementAPI/resource routes.
//
// The management UI shows a "Start <name> Login" card for every plugin with an
// AuthProvider, but StartLogin used to fail for API-key providers, surfacing
// only as a generic "failed to generate authorization url". This helper makes
// StartLogin succeed with no host change:
//
//  1. StartLogin mints a random session and returns the URL of a kit-served
//     login page: a browser-navigable resource route (GET, unauthenticated,
//     served by the host at /v0/resource/plugins/<id>/login?state=...). The
//     URL keeps the host callback origin (a loopback address on a default
//     host): a browser on the host machine opens it directly, while a remote
//     operator swaps in the host's public base — the page path itself is
//     origin-independent. The plugin cannot learn the public origin without
//     a host change, so the returned URL stays absolute (non-browser clients
//     need it) rather than root-relative.
//  2. The page carries no secrets. It shows copy-paste shell commands that
//     read the management key and the API key with hidden input and POST
//     them to a kit-served management route, so neither secret lands in the
//     command text, shell history, a URL, or a log line. The POST stays
//     behind the host's existing management authentication, and the key
//     travels in the JSON body only. There is deliberately no HTML form: a
//     browser cannot present management credentials on its own, and a form
//     would train operators to paste their management key into a
//     plugin-served page (the plugin would see that key in the request
//     headers the host forwards).
//  3. The submit handler runs the provider's full ParseAuth pipeline,
//     including provider validation and the duplicate-key guard, against a
//     deterministic file name derived from the key digest, so submitting the
//     same key twice is idempotent instead of a duplicate. The validated
//     AuthData is kept in the memory-only session; PollLogin delivers it to
//     exactly one poller, then keeps a credential-free tombstone until expiry
//     so a concurrent second poller waits instead of failing the host login.
//     Raw keys are never written to disk by this flow and never leave the
//     request body.
//
// Sessions are process-local with a TTL, so a plugin restart or expiry
// surfaces as a poll error telling the operator to start a new login.
const (
	defaultLoginSessionTTL = 15 * time.Minute
	maxLoginSessions       = 128
	maxLoginSubmitBody     = 64 << 10
	loginPageRoute         = "login"
	loginSubmitPrefix      = "plugin-login/"
	defaultManagementBase  = "/v0/management"
)

// APIKeyLoginField describes one provider-owned auth field, beyond api_key
// and label, that the login page documents and the submit endpoint accepts.
type APIKeyLoginField struct {
	// Name is the JSON key accepted by the submit endpoint.
	Name string
	// Label is the short human-readable name shown on the login page.
	Label string
	// Description explains the field on the login page.
	Description string
	// Required rejects submissions that omit the field.
	Required bool
}

// APIKeyLoginConfig wires provider behavior into the login helper.
type APIKeyLoginConfig struct {
	// Provider is the provider key; it must match the APIKeyAuth provider.
	Provider string
	// Parse is the provider's full ParseAuth pipeline, including validation
	// and the duplicate-key guard.
	Parse func(context.Context, pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error)
	// Fields documents extra provider-owned auth fields.
	Fields []APIKeyLoginField
	// SessionTTL bounds a pending login; defaults to 15 minutes.
	SessionTTL time.Duration
}

type loginSession struct {
	expiresAt time.Time
	pageURL   string
	authDir   string
	auth      pluginapi.AuthData
	auths     []pluginapi.AuthData
	done      bool
	// consumed marks a session whose credentials were already delivered to
	// exactly one poller. The entry is kept until expiry so a concurrent
	// second poller gets a harmless pending instead of an error that would
	// poison the host login session; stored credentials are zeroed at once
	// to preserve single delivery.
	consumed bool
}

// APIKeyLogin serves one provider's API-key login flow. It implements the
// StartLogin/PollLogin half of pluginapi.AuthProvider (wired through
// APIKeyAuth.Login) and the pluginapi.ManagementAPI plus
// pluginapi.ManagementHandler halves for Capabilities.ManagementAPI.
type APIKeyLogin struct {
	provider string
	parse    func(context.Context, pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error)
	fields   []APIKeyLoginField
	byName   map[string]APIKeyLoginField
	ttl      time.Duration

	mu           sync.Mutex
	sessions     map[string]*loginSession
	mgmtBase     string
	resourceBase string
}

// NewAPIKeyLogin validates the config. Parse must be non-nil: the submit
// endpoint only stores keys that pass the provider's own checks.
//
// Wire the result into both APIKeyAuth.Login and the Definition's
// Capabilities.ManagementAPI: the first serves StartLogin/PollLogin, the
// second registers the login page and key-submit routes. Setting only one
// leaves StartLogin failing with login_unavailable.
func NewAPIKeyLogin(cfg APIKeyLoginConfig) (*APIKeyLogin, error) {
	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" || strings.Contains(provider, "..") || strings.ContainsAny(provider, "/\\ \t\r\n") {
		return nil, rpcError("invalid_login", "login provider must be a bare provider key", 500)
	}
	if cfg.Parse == nil {
		return nil, rpcError("invalid_login", "login requires the provider ParseAuth pipeline", 500)
	}
	byName := make(map[string]APIKeyLoginField, len(cfg.Fields))
	for _, f := range cfg.Fields {
		name := strings.TrimSpace(f.Name)
		if name == "" {
			return nil, rpcError("invalid_login", "login field names must be nonempty", 500)
		}
		switch name {
		case "type", "id", "state", "api_key", "label":
			return nil, rpcError("invalid_login", "login field name is reserved", 500)
		}
		if _, dup := byName[name]; dup {
			return nil, rpcError("invalid_login", "duplicate login field name", 500)
		}
		f.Name = name
		byName[name] = f
	}
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = defaultLoginSessionTTL
	}
	fields := make([]APIKeyLoginField, 0, len(cfg.Fields))
	for _, f := range cfg.Fields {
		f.Name = strings.TrimSpace(f.Name)
		fields = append(fields, f)
	}
	return &APIKeyLogin{provider: provider, parse: cfg.Parse, fields: fields, byName: byName, ttl: ttl,
		sessions: make(map[string]*loginSession), mgmtBase: defaultManagementBase}, nil
}

// loginOrigin derives the scheme://host origin of the login page from the
// host callback base URL the host passes to StartLogin. Only the origin is
// kept: the callback path itself is host-local and meaningless to the
// operator's browser.
func loginOrigin(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", rpcError("login_unavailable", "host login base URL is not a URL", 500)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(u.Host, " \t\r\n'\"") {
		return "", rpcError("login_unavailable", "host login base URL is missing", 500)
	}
	return u.Scheme + "://" + u.Host, nil
}

func (l *APIKeyLogin) evictLocked(now time.Time) {
	for state, s := range l.sessions {
		if !now.Before(s.expiresAt) {
			delete(l.sessions, state)
		}
	}
	for len(l.sessions) >= maxLoginSessions {
		oldest := ""
		for state, s := range l.sessions {
			if oldest == "" || s.expiresAt.Before(l.sessions[oldest].expiresAt) {
				oldest = state
			}
		}
		if oldest == "" {
			return
		}
		delete(l.sessions, oldest)
	}
}

// StartLogin mints a pending session and returns the login page URL. It fails
// only when the host gave no usable base URL or the management routes were
// never registered, in which case the host keeps its generic error.
func (l *APIKeyLogin) StartLogin(_ context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	if l == nil {
		return pluginapi.AuthLoginStartResponse{}, rpcError("api_key_only", "API-key only: create an auth JSON file", 400)
	}
	origin, err := loginOrigin(req.BaseURL)
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return pluginapi.AuthLoginStartResponse{}, rpcError("login_unavailable", "cannot mint login state", 500)
	}
	state := hex.EncodeToString(raw[:])
	now := time.Now()
	l.mu.Lock()
	if strings.TrimSpace(l.resourceBase) == "" {
		l.mu.Unlock()
		return pluginapi.AuthLoginStartResponse{}, rpcError("login_unavailable", "login routes are not registered", 500)
	}
	l.evictLocked(now)
	pageURL := origin + l.resourceBase + "/" + loginPageRoute + "?state=" + url.QueryEscape(state)
	l.sessions[state] = &loginSession{expiresAt: now.Add(l.ttl), pageURL: pageURL, authDir: strings.TrimSpace(req.Host.AuthDir)}
	l.mu.Unlock()
	return pluginapi.AuthLoginStartResponse{Provider: l.provider, URL: pageURL, State: state, ExpiresAt: now.Add(l.ttl)}, nil
}

// PollLogin reports pending until the submit endpoint completes the session,
// then delivers the validated AuthData to exactly one poller. A concurrent
// second poller gets a credential-free pending (not an error) until the
// consumed entry expires, so it cannot poison the host login session.
func (l *APIKeyLogin) PollLogin(_ context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	if l == nil {
		return pluginapi.AuthLoginPollResponse{}, rpcError("api_key_only", "API-key only: create an auth JSON file", 400)
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[req.State]
	if !ok || !now.Before(s.expiresAt) {
		delete(l.sessions, req.State)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login session expired or not found; start a new login"}, nil
	}
	if s.consumed {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "login was already collected by another poll; finishing"}, nil
	}
	if !s.done {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for the API key submission from " + s.pageURL}, nil
	}
	auth, auths := s.auth, s.auths
	s.consumed = true
	s.auth = pluginapi.AuthData{}
	s.auths = nil
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "login complete", Auth: auth, Auths: auths}, nil
}

// RegisterManagement captures the host's route bases and declares the login
// page (resource route) and the key submit endpoint (management route). The
// submit path is namespaced per provider because management routes share one
// global namespace across plugins.
func (l *APIKeyLogin) RegisterManagement(_ context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	if l == nil {
		return pluginapi.ManagementRegistrationResponse{}, rpcError("login_unavailable", "login is not configured", 500)
	}
	base := strings.TrimSpace(req.BasePath)
	if base == "" {
		base = defaultManagementBase
	}
	l.mu.Lock()
	l.mgmtBase = strings.TrimRight(base, "/")
	l.resourceBase = strings.TrimRight(strings.TrimSpace(req.ResourceBasePath), "/")
	l.mu.Unlock()
	// Handlers stay nil on the wire: ManagementHandler is a non-empty
	// interface, so any JSON value for it (including {}) fails to decode
	// on the host (rpcPluginAdapter.RegisterManagement decodes into the
	// SDK route structs) and no route would register. The host adapter
	// assigns its own Handler after decoding; the kit dispatches incoming
	// management.handle calls itself in HandleManagement below.
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{{
			Method:      http.MethodPost,
			Path:        loginSubmitPrefix + l.provider,
			Description: "Submit an API key for a pending " + l.provider + " login started from the management UI.",
		}},
		Resources: []pluginapi.ResourceRoute{{
			Path:        loginPageRoute,
			Description: "Enter an API key to complete a pending login started from the management UI.",
		}},
	}, nil
}

// HandleManagement dispatches the declared routes by exact method and full
// path. Request headers are intentionally ignored: the host forwards
// management credentials with the request, and the login flow must never
// read, log, or echo them.
func (l *APIKeyLogin) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if l == nil {
		return pluginapi.ManagementResponse{}, rpcError("login_unavailable", "login is not configured", 500)
	}
	l.mu.Lock()
	mgmtBase, resourceBase := l.mgmtBase, l.resourceBase
	l.mu.Unlock()
	if mgmtBase == "" {
		mgmtBase = defaultManagementBase
	}
	switch {
	case req.Method == http.MethodGet && req.Path == resourceBase+"/"+loginPageRoute && resourceBase != "":
		return l.servePage(req), nil
	case req.Path == mgmtBase+"/"+loginSubmitPrefix+l.provider:
		if req.Method != http.MethodPost {
			return loginJSON(http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"code": "method_not_allowed", "message": "use POST"}}), nil
		}
		return l.serveSubmit(ctx, req), nil
	default:
		return loginJSON(http.StatusNotFound, map[string]any{"error": map[string]any{"code": "unknown_route", "message": "no login route"}}), nil
	}
}

func loginJSON(status int, v any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(v)
	if err != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusInternalServerError,
			Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"error":{"code":"encoding_error","message":"response encoding failed"}}`)}
	}
	return pluginapi.ManagementResponse{StatusCode: status,
		Headers: http.Header{"Content-Type": {"application/json"}}, Body: raw}
}

func loginJSONError(status int, code, message string) pluginapi.ManagementResponse {
	return loginJSON(status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func (l *APIKeyLogin) lookup(state string) *loginSession {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[state]
	if !ok || !now.Before(s.expiresAt) {
		delete(l.sessions, state)
		return nil
	}
	return s
}

// servePage renders the login page for a pending session. The page holds the
// session state and a copy-paste curl command only; the API key and the
// management key are typed into the operator's terminal, never into this page.
func (l *APIKeyLogin) servePage(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	l.mu.Lock()
	mgmtBase := l.mgmtBase
	l.mu.Unlock()
	if mgmtBase == "" {
		mgmtBase = defaultManagementBase
	}
	submitURL := mgmtBase + "/" + loginSubmitPrefix + l.provider
	state := req.Query.Get("state")
	s := l.lookup(state)
	var b strings.Builder
	b.WriteString("<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	b.WriteString("<title>" + html.EscapeString(l.provider) + " API key login</title></head><body>")
	b.WriteString("<h1>" + html.EscapeString(l.provider) + " API key login</h1>")
	if s == nil {
		b.WriteString("<p>This login session expired or does not exist. In the management UI, click <b>Start " +
			html.EscapeString(l.provider) + " Login</b> again to get a fresh login page.</p>")
	} else {
		b.WriteString("<p>Run the commands below from any machine that reaches the CLIProxy host. " +
			"Set CLIPROXY_BASE to the host's public base URL (for example http://host:port); " +
			"the secrets are read with hidden input and never appear in the commands, shell history, URLs, or logs. " +
			"The key travels in the POST body only.</p>")
		submitCmd := "read -s -p 'Management key: ' CLIPROXY_MANAGEMENT_KEY; echo\n" +
			"read -s -p '" + l.provider + " API key: ' PLUGIN_API_KEY; echo\n" +
			`curl -X POST "$CLIPROXY_BASE` + submitURL + `" ` +
			"\\\n  -H \"Authorization: Bearer $CLIPROXY_MANAGEMENT_KEY\" \\\n  -H 'Content-Type: application/json' \\\n" +
			`  -d "$(printf '{"state":"` + state + `","api_key":"%s"}' "$PLUGIN_API_KEY")"`
		b.WriteString("<pre>" + html.EscapeString(submitCmd) + "</pre>")
		b.WriteString("<p>Keep polling this login in the management UI; it completes a few seconds after the POST succeeds. This page expires " +
			html.EscapeString(s.expiresAt.UTC().Format(time.RFC3339)) + ".</p>")
		if len(l.fields) > 0 {
			b.WriteString("<h2>Optional provider fields</h2><ul>")
			for _, f := range l.fields {
				item := html.EscapeString(f.Name)
				if strings.TrimSpace(f.Label) != "" {
					item += " (" + html.EscapeString(f.Label) + ")"
				}
				if strings.TrimSpace(f.Description) != "" {
					item += ": " + html.EscapeString(f.Description)
				}
				if f.Required {
					item += " <b>(required)</b>"
				}
				b.WriteString("<li>" + item + "</li>")
			}
			b.WriteString("</ul>")
		}
		b.WriteString("<p>Your management key authenticates you to the host; this page never asks for it and the plugin ignores request headers.</p>")
	}
	b.WriteString("</body></html>")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK,
		Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: []byte(b.String())}
}

// serveSubmit validates one key submission through the provider's ParseAuth
// pipeline and completes the pending session. Errors never echo the key:
// provider RPC errors are passed through with the submitted key scrubbed,
// and anything else is already redacted by MapError.
func (l *APIKeyLogin) serveSubmit(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(req.Body) > maxLoginSubmitBody {
		return loginJSONError(http.StatusBadRequest, "invalid_request", "request body too large")
	}
	fields, err := object(req.Body)
	if err != nil {
		return loginJSONError(http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
	}
	state, _ := fields["state"].(string)
	s := l.lookup(state)
	if s == nil {
		return loginJSONError(http.StatusBadRequest, "login_expired", "login session expired or not found; start a new login")
	}
	key, _ := fields["api_key"].(string)
	key = strings.TrimSpace(key)
	if _, ok := fields["api_key"].(string); !ok || key == "" {
		return loginJSONError(http.StatusBadRequest, "invalid_auth", "a nonempty api_key is required")
	}
	if v, ok := fields["label"]; ok {
		if _, ok := v.(string); !ok {
			return loginJSONError(http.StatusBadRequest, "invalid_auth", "label must be a string")
		}
	}
	candidate := map[string]any{"type": l.provider, "api_key": key}
	for name, value := range fields {
		switch name {
		case "state", "api_key":
		case "label":
			if strings.TrimSpace(value.(string)) != "" {
				candidate[name] = value
			}
		default:
			if _, ok := l.byName[name]; !ok {
				return loginJSONError(http.StatusBadRequest, "invalid_request", "unknown auth field "+name)
			}
			candidate[name] = value
		}
	}
	for _, f := range l.fields {
		if f.Required {
			if _, ok := candidate[f.Name]; !ok {
				return loginJSONError(http.StatusBadRequest, "invalid_auth", "missing required field "+f.Name)
			}
		}
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		return loginJSONError(http.StatusBadRequest, "invalid_auth", "cannot encode auth storage")
	}
	// The deterministic, key-derived file name makes a repeated submission of
	// the same key overwrite the same pending session instead of tripping the
	// provider's duplicate-key guard.
	digest := sha256.Sum256([]byte(key))
	fileName := l.provider + "-" + hex.EncodeToString(digest[:6]) + ".json"
	path := fileName
	if s.authDir != "" {
		path = strings.TrimRight(s.authDir, "/") + "/" + fileName
	}
	out, err := l.parse(ctx, pluginapi.AuthParseRequest{Provider: l.provider, FileName: fileName, Path: path, RawJSON: raw})
	if err != nil {
		mapped := MapError(err)
		status := mapped.HTTPStatus
		if status < 400 || status > 599 {
			status = http.StatusBadRequest
		}
		message := mapped.Message
		if key != "" {
			message = strings.ReplaceAll(message, key, "[redacted]")
		}
		return loginJSONError(status, mapped.Code, message)
	}
	if !out.Handled {
		return loginJSONError(http.StatusBadRequest, "invalid_auth", "provider did not accept the submission")
	}
	auth := out.Auth
	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = fileName
	}
	if strings.TrimSpace(auth.Provider) == "" {
		auth.Provider = l.provider
	}
	// Revalidate under the final lock: the session may have expired (or
	// been evicted) while ParseAuth ran. Committing into a detached session
	// would return success for a login that can never complete.
	l.mu.Lock()
	current, ok := l.sessions[state]
	switch {
	case !ok || current != s:
		l.mu.Unlock()
		return loginJSONError(http.StatusBadRequest, "login_expired", "login session expired or not found; start a new login")
	case !time.Now().Before(current.expiresAt):
		delete(l.sessions, state)
		l.mu.Unlock()
		return loginJSONError(http.StatusBadRequest, "login_expired", "login session expired or not found; start a new login")
	default:
		s.auth, s.auths, s.done = auth, out.Auths, true
	}
	l.mu.Unlock()
	return loginJSON(http.StatusOK, map[string]any{"ok": true})
}
