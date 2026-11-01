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
	"regexp"
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
// StartLogin succeed with no host change. The panel renders the plugin card
// with its standard OAuth dialog (open-link step, callback-URL box, status
// polling, success view); the API-key flow below completes through that same
// dialog's polling and success states.
//
//  1. StartLogin mints a random session and returns a ROOT-RELATIVE login page
//     URL (resource route, GET, unauthenticated, served by the host under
//     /v0/resource/plugins/<id>/login?state=...). The host passes the URL to
//     the panel verbatim, and the panel opens it against its own origin —
//     which is the host itself, since the host serves the panel. An absolute
//     URL can only reuse the loopback origin the host hands to StartLogin,
//     which a remote operator's browser cannot load; the relative URL resolves
//     wherever the panel runs. (The panel tolerates relative URLs: its
//     link-splitting falls back to plain text and window.open resolves them.)
//  2. The page carries no secrets. It shows copy-paste shell commands that
//     read the management key and the API key with hidden input and POST
//     them to a kit-served management route, so neither secret lands in the
//     pasted command text, shell history, a URL, or a log line. Both secrets
//     also stay out of process arguments: the bearer header travels in a
//     process-substitution file and the JSON body over stdin (heredoc), so
//     `ps` and /proc show only file descriptors and `-` instead of the key.
//     The page prompts for every declared provider field (at minimum the
//     required ones) and shell-escapes each value with builtins only, so
//     quotes, backslashes, and newlines cannot corrupt the JSON body. The
//     POST stays
//     behind the host's existing management authentication, and the key
//     travels in the JSON body only. There is deliberately no HTML form: a
//     browser cannot present management credentials on its own, and a form
//     would train operators to paste their management key into a
//     plugin-served page (the plugin would see that key in the request
//     headers the host forwards).
//  3. The submit handler runs the provider's full ParseAuth pipeline,
//     including provider validation and the duplicate-key guard, against a
//     deterministic file name derived from the key digest (plus any fields
//     declared in FilenameFields, for identities that span extra fields),
//     so submitting the same identity twice is idempotent instead of a
//     duplicate. That validation parse is marked provisional
//     (ProvisionalParseExpiry), so an ownership guard reserves the key only
//     until the session expires instead of committing it for a file the
//     host may never save. The validated
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

// loginProviderPattern bounds Provider to shell- and path-safe characters.
// The provider key is interpolated into the login page's shell prompts (inside
// single quotes), the management submit path, and the deterministic auth file
// name, so anything outside [A-Za-z0-9-] (backslash, $, backticks, quotes,
// slashes, dots) is rejected at config time instead of only documented.
var loginProviderPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// loginFieldPattern bounds field names to [A-Za-z0-9_]. A name is interpolated
// into the login page's unquoted heredoc body, so anything else (backslash, $,
// backticks) would be interpreted by the operator's shell instead of arriving
// literally.
var loginFieldPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// APIKeyLoginField describes one provider-owned auth field, beyond api_key
// and label, that the login page documents and the submit endpoint accepts.
type APIKeyLoginField struct {
	// Name is the JSON key accepted by the submit endpoint. NewAPIKeyLogin
	// enforces the [A-Za-z0-9_] charset (see loginFieldPattern).
	Name string
	// Label is the short human-readable name shown on the login page.
	Label string
	// Description explains the field on the login page.
	Description string
	// Required rejects submissions that omit the field.
	Required bool
	// Secret reads the value with hidden terminal input (read -rs), like
	// the main API key, instead of the plain read -r used for visible
	// fields. Set it for extra keys and tokens (e.g. a separate quota or
	// analytics key) so they never echo while typed. Secret values are
	// also scrubbed from provider error messages, like the API key.
	Secret bool
	// LowercaseForFilename lower-cases the submitted value before it joins
	// the FilenameFields digest, without changing the value handed to
	// Parse. Set it when the field is also in FilenameFields and the
	// provider's identity is case-insensitive for it (for example a hex
	// account ID that Parse canonicalizes to lowercase): otherwise two
	// case spellings of one identity would mint different file names while
	// sharing the provider's canonical identity, and a repeat login would
	// trip the duplicate guard instead of staying idempotent. It has no
	// effect for fields not listed in FilenameFields.
	LowercaseForFilename bool
}

// APIKeyLoginConfig wires provider behavior into the login helper.
type APIKeyLoginConfig struct {
	// Provider is the provider key; it must match the APIKeyAuth provider.
	// NewAPIKeyLogin enforces the shell-safe charset (see
	// loginProviderPattern).
	Provider string
	// Parse is the provider's full ParseAuth pipeline, including validation
	// and the duplicate-key guard. It runs as a provisional parse (see
	// ProvisionalParseExpiry): the guard must not commit filename ownership
	// for a login that may never be saved. KeyOwnership handles this; a
	// hand-rolled guard should check the context it is handed.
	Parse func(context.Context, pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error)
	// Fields documents extra provider-owned auth fields.
	Fields []APIKeyLoginField
	// FilenameFields lists declared Fields whose submitted values, in
	// addition to the API key, determine the deterministic auth file name
	// passed to Parse. Providers whose identity spans extra fields (for
	// example an account ID plus a token that may be reused across
	// accounts) must list those fields here: with the key-only default, two
	// identities sharing a key would mint the same file name and the second
	// ParseAuth would overwrite the first identity's ownership entry.
	// Empty (the default) keeps the historical key-only derivation, so
	// existing providers' file names are unchanged. Values are hashed as
	// submitted (strings trimmed, plus lower-cased for fields declaring
	// LowercaseForFilename); other provider-side normalization inside Parse
	// is not reflected, so identity fields should be submitted in canonical
	// form or declare the case fold. A listed optional field that is omitted
	// or blank contributes no value. Providers using FilenameFields
	// should also derive distinct Auth IDs for distinct identities in Parse.
	FilenameFields []string
	// SessionTTL bounds a pending login; defaults to 15 minutes.
	SessionTTL time.Duration
}

type loginSession struct {
	expiresAt time.Time
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
// loginFilenameField is one resolved FilenameFields entry: the declared
// field name plus whether its value is lower-cased before hashing.
type loginFilenameField struct {
	name      string
	lowercase bool
}

type APIKeyLogin struct {
	provider       string
	parse          func(context.Context, pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error)
	fields         []APIKeyLoginField
	filenameFields []loginFilenameField
	byName         map[string]APIKeyLoginField
	ttl            time.Duration

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
	if !loginProviderPattern.MatchString(provider) {
		return nil, rpcError("invalid_login", "login provider must match [A-Za-z0-9][A-Za-z0-9-]{0,63}", 500)
	}
	if cfg.Parse == nil {
		return nil, rpcError("invalid_login", "login requires the provider ParseAuth pipeline", 500)
	}
	byName := make(map[string]APIKeyLoginField, len(cfg.Fields))
	for _, f := range cfg.Fields {
		name := strings.TrimSpace(f.Name)
		if !loginFieldPattern.MatchString(name) {
			return nil, rpcError("invalid_login", "login field names must match [A-Za-z0-9_]{1,64}", 500)
		}
		switch name {
		case "type", "id", "state", "api_key", "label":
			return nil, rpcError("invalid_login", "login field name is reserved", 500)
		case "weight", "priority", "disabled":
			// The page submits every field answer as a JSON string, but
			// ParseAuth requires integers for weight/priority and a
			// boolean for disabled, so declaring one could never submit
			// successfully. Reserve them to fail fast at config time.
			return nil, rpcError("invalid_login", "login field name is reserved", 500)
		}
		if _, dup := byName[name]; dup {
			return nil, rpcError("invalid_login", "duplicate login field name", 500)
		}
		f.Name = name
		byName[name] = f
	}
	// The login page maps each field to a PLUGIN_FIELD_<NAME> shell variable.
	// Two fields that sanitize to the same variable would overwrite each
	// other in the operator's shell, so reject the config here.
	seenVars := make(map[string]string, len(cfg.Fields))
	for _, f := range cfg.Fields {
		v := loginShellVar(strings.TrimSpace(f.Name))
		if prev, dup := seenVars[v]; dup {
			return nil, rpcError("invalid_login", "login fields "+prev+" and "+strings.TrimSpace(f.Name)+" share a shell variable", 500)
		}
		seenVars[v] = strings.TrimSpace(f.Name)
	}
	// FilenameFields must name declared Fields: the submit endpoint rejects
	// undeclared fields, so an undeclared name could never contribute a
	// value and would silently keep the colliding key-only derivation.
	// api_key is rejected rather than ignored because it is always part of
	// the derivation; listing it would suggest it is somehow optional.
	filenameFields := make([]loginFilenameField, 0, len(cfg.FilenameFields))
	seenFilenameFields := make(map[string]bool, len(cfg.FilenameFields))
	for _, rawName := range cfg.FilenameFields {
		name := strings.TrimSpace(rawName)
		if name == "api_key" {
			return nil, rpcError("invalid_login", "api_key is always a login filename input and must not be listed in FilenameFields", 500)
		}
		field, ok := byName[name]
		if !ok {
			return nil, rpcError("invalid_login", "login filename field must name a declared login field", 500)
		}
		if seenFilenameFields[name] {
			return nil, rpcError("invalid_login", "duplicate login filename field", 500)
		}
		seenFilenameFields[name] = true
		filenameFields = append(filenameFields, loginFilenameField{name: name, lowercase: field.LowercaseForFilename})
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
	return &APIKeyLogin{provider: provider, parse: cfg.Parse, fields: fields, filenameFields: filenameFields,
		byName: byName, ttl: ttl, sessions: make(map[string]*loginSession), mgmtBase: defaultManagementBase}, nil
}

// loginFileName derives the deterministic auth file name for one login
// identity. With no filename fields it is the historical key-only digest, so
// existing providers keep their current file names. With filename fields,
// each field's name and JSON-encoded submitted value (strings trimmed, and
// lower-cased when the field declares it; an omitted optional field as JSON
// null) is hashed after the key with NUL separators, so distinct identities
// that share a key mint distinct names and no split of values across fields
// can alias another identity. The digest is truncated exactly like the
// key-only form, and neither the key nor any field value appears in the name.
func loginFileName(provider, key string, filenameFields []loginFilenameField, candidate map[string]any) (string, error) {
	if len(filenameFields) == 0 {
		digest := sha256.Sum256([]byte(key))
		return provider + "-" + hex.EncodeToString(digest[:6]) + ".json", nil
	}
	h := sha256.New()
	h.Write([]byte(key))
	for _, field := range filenameFields {
		value := candidate[field.name]
		if s, ok := value.(string); ok {
			s = strings.TrimSpace(s)
			if field.lowercase {
				s = strings.ToLower(s)
			}
			value = s
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		h.Write([]byte{0})
		h.Write([]byte(field.name))
		h.Write([]byte{0})
		h.Write(raw)
	}
	digest := h.Sum(nil)
	return provider + "-" + hex.EncodeToString(digest[:6]) + ".json", nil
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

// StartLogin mints a pending session and returns the login page URL. The URL
// is root-relative on purpose: the host callback base URL the host passes in
// is loopback-only, so absolutizing with it would hand the panel a link the
// operator's browser cannot load. The panel opens the relative URL against
// its own origin (the host, which serves the panel), so the page loads both
// locally and remotely. StartLogin fails only when the management routes were
// never registered, in which case the host keeps its generic error.
func (l *APIKeyLogin) StartLogin(_ context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	if l == nil {
		return pluginapi.AuthLoginStartResponse{}, rpcError("api_key_only", "API-key only: create an auth JSON file", 400)
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
	pageURL := l.resourceBase + "/" + loginPageRoute + "?state=" + url.QueryEscape(state)
	l.sessions[state] = &loginSession{expiresAt: now.Add(l.ttl), authDir: strings.TrimSpace(req.Host.AuthDir)}
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
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for the API key submission: open the login link, run its command block, then return here; this login completes automatically"}, nil
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
	l.mgmtBase = routeBase(strings.TrimSpace(base))
	l.resourceBase = routeBase(strings.TrimSpace(req.ResourceBasePath))
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

// routeBase normalizes a host route base to a root-relative path: trimmed,
// no trailing slash, always starting with "/". A base without a leading slash
// would make StartLogin return a relative URL that resolves against the
// panel's route path instead of its origin, failing exactly the remote
// operators the relative URL is for. Empty stays empty so StartLogin keeps
// failing with login_unavailable when routes were never registered.
//
// The base is interpolated into the operator's pasted shell block inside
// double quotes, where ", `, $, and \ would break out and execute, and a
// leading "//" would resolve as an off-origin URL in the panel. Legitimate
// bases are plain path prefixes, so a base containing any of those (or a
// newline) is rejected to "" and StartLogin fails with login_unavailable
// instead of serving a hostile block.
func routeBase(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base != "" && !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	if strings.ContainsAny(base, "\"`$\\\n\r") || strings.HasPrefix(base, "//") {
		return ""
	}
	return base
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

// loginShellVar maps a provider field name to the PLUGIN_FIELD_<NAME> shell
// variable the login page reads it into. Names are upper-cased and anything
// outside [A-Z0-9] becomes "_"; NewAPIKeyLogin rejects fields that collide.
func loginShellVar(name string) string {
	var b strings.Builder
	b.WriteString("PLUGIN_FIELD_")
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// loginShellSingle quotes text for embedding in a single-quoted shell string
// (prompts carry provider and field names, which the config does not constrain
// to shell-safe characters).
func loginShellSingle(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// loginEscapeLine emits one bash/zsh line that copies VAR into ESC with
// JSON-safe escaping (backslash, double quote, newline, CR, tab). Every
// expansion is a shell builtin, so the values never appear in a child
// process's arguments; the failure mode without it is a safe 400 from the
// submit endpoint, never a wrong key.
func loginEscapeLine(varName, escName string) string {
	return escName + "=${" + varName + "//\\\\/\\\\\\\\}; " +
		escName + "=${" + escName + "//\\\"/\\\\\\\"}; " +
		escName + "=${" + escName + "//$'\\n'/\\\\n}; " +
		escName + "=${" + escName + "//$'\\r'/\\\\r}; " +
		escName + "=${" + escName + "//$'\\t'/\\\\t}"
}

// loginScrubSecret removes a secret and its URL-encoded and JSON-escaped
// forms from an operator-facing message. Blank secrets scrub nothing.
func loginScrubSecret(message, secret string) string {
	if secret == "" {
		return message
	}
	message = strings.ReplaceAll(message, secret, "[redacted]")
	if enc := url.QueryEscape(secret); enc != secret {
		message = strings.ReplaceAll(message, enc, "[redacted]")
	}
	if esc := loginJSONString(secret); esc != secret {
		message = strings.ReplaceAll(message, esc, "[redacted]")
	}
	return message
}

// loginJSONString escapes a server-side string (a declared field name) for
// embedding in the page's JSON body template.
func loginJSONString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return r.Replace(s)
}

// servePage renders the login page for a pending session. The page holds the
// session state and a copy-paste shell block only; the API key and the
// management key are typed into the operator's terminal, never into this page.
// The block is wrapped in braces so it parses as one compound command: when a
// paste reaches the shell line by line (bracketed paste off), an unwrapped
// read -rs would eat the next pasted line as its input and the operator's
// real key would then run as a command, landing in history and on stderr.
// The block is written for bash and zsh (prompt via printf, hidden raw input
// via read -rs for the keys and Secret fields, plain raw input via read -r
// for visible fields: raw mode keeps backslashes in keys intact, and -p is
// avoided because zsh gives it another meaning). Both
// secrets travel over stdin — the bearer header in a process-substitution
// file, the JSON body in a heredoc — so neither appears in process arguments,
// shell history, a URL, or a log line.
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
	b.WriteString("<title>" + html.EscapeString(l.provider) + " login</title>")
	b.WriteString("<style>body{font-family:system-ui,-apple-system,sans-serif;max-width:44rem;margin:2rem auto;padding:0 1rem;line-height:1.5}" +
		"pre{background:#f4f4f5;padding:1rem;overflow-x:auto;border-radius:.5rem}" +
		"ol.steps{padding-left:1.25rem}ol.steps>li{margin:.5rem 0}.note{color:#52525b}</style></head><body>")
	b.WriteString("<h1>" + html.EscapeString(l.provider) + " login</h1>")
	if s == nil {
		b.WriteString("<p>This login session expired or does not exist. In the management UI, click <b>Start " +
			html.EscapeString(l.provider) + " Login</b> again to get a fresh login page.</p>")
	} else {
		b.WriteString("<ol class=\"steps\">" +
			"<li><b>Copy</b> the command block below.</li>" +
			"<li><b>Paste it into bash or zsh</b> on any machine that reaches the CLIProxy host. " +
			"Set CLIPROXY_BASE to the host's public base URL (for example https://host:port).</li>" +
			"<li><b>Return to the management UI.</b> This login completes automatically a few seconds after the command succeeds.</li>" +
			"</ol>" +
			"<p class=\"note\">The panel's callback-URL box is only for OAuth providers and is not used for API-key logins; leave it alone.</p>" +
			"<p>On a remote host always use https:// so the management key and the API key are encrypted in transit; " +
			"plain http:// is only acceptable on a loopback address you trust. " +
			"The secrets are read with hidden input and travel over stdin only: they never appear in the pasted text, " +
			"shell history, process arguments, URLs, or logs. " +
			"The key travels in the POST body only.</p>")
		var cmd strings.Builder
		cmd.WriteString("{\n")
		cmd.WriteString("printf 'Management key: '; read -rs CLIPROXY_MANAGEMENT_KEY; echo\n")
		cmd.WriteString("printf '" + loginShellSingle(l.provider) + " API key: '; read -rs PLUGIN_API_KEY; echo\n")
		for _, f := range l.fields {
			prompt := strings.TrimSpace(f.Label)
			if prompt == "" {
				prompt = f.Name
			}
			if f.Required {
				prompt += " (required)"
			} else {
				prompt += " (optional)"
			}
			readFlags := "read -r "
			if f.Secret {
				readFlags = "read -rs "
			}
			cmd.WriteString("printf '" + loginShellSingle(prompt) + ": '; " + readFlags + loginShellVar(f.Name) + "; echo\n")
		}
		// Header values must not span lines: strip CR/LF from the management
		// key with builtins before it reaches the header file. The header
		// takes the raw key (no JSON escaping); the body takes the escaped
		// copies below.
		cmd.WriteString("CLEAN_MGMT=${CLIPROXY_MANAGEMENT_KEY//$'\\n'/}; CLEAN_MGMT=${CLEAN_MGMT//$'\\r'/}\n")
		cmd.WriteString(loginEscapeLine("PLUGIN_API_KEY", "ESC_API_KEY") + "\n")
		escVars := []string{"ESC_API_KEY"}
		for _, f := range l.fields {
			v := loginShellVar(f.Name)
			cmd.WriteString(loginEscapeLine(v, "ESC_"+strings.TrimPrefix(v, "PLUGIN_")) + "\n")
			escVars = append(escVars, "ESC_"+strings.TrimPrefix(v, "PLUGIN_"))
		}
		var body strings.Builder
		body.WriteString(`{"state":"` + loginJSONString(state) + `","api_key":"$ESC_API_KEY"`)
		for i, f := range l.fields {
			body.WriteString(`,"` + loginJSONString(f.Name) + `":"$` + escVars[i+1] + `"`)
		}
		body.WriteString("}")
		cmd.WriteString(`curl -X POST "$CLIPROXY_BASE` + submitURL + `" ` +
			"\\\n  -H @<(printf '%s' \"Authorization: Bearer $CLEAN_MGMT\") \\\n  -H 'Content-Type: application/json' \\\n" +
			"  --data-binary @- <<EOF\n" + body.String() + "\nEOF\n")
		unset := "unset CLIPROXY_MANAGEMENT_KEY PLUGIN_API_KEY CLEAN_MGMT ESC_API_KEY"
		for _, f := range l.fields {
			v := loginShellVar(f.Name)
			unset += " " + v + " ESC_" + strings.TrimPrefix(v, "PLUGIN_")
		}
		cmd.WriteString(unset + "\n}\n")
		b.WriteString("<pre>" + html.EscapeString(cmd.String()) + "</pre>")
		b.WriteString("<p>Leave optional fields blank to skip them. This page expires " +
			html.EscapeString(s.expiresAt.UTC().Format(time.RFC3339)) + ".</p>")
		if len(l.fields) > 0 {
			b.WriteString("<h2>Provider fields</h2><ul>")
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
				} else {
					item += " (optional)"
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
			f, ok := l.byName[name]
			if !ok {
				return loginJSONError(http.StatusBadRequest, "invalid_request", "unknown auth field "+name)
			}
			// A blank prompt answer for an optional field means "skip it":
			// omit it instead of storing an empty string. A blank required
			// field is a missing field.
			if str, isStr := value.(string); isStr && strings.TrimSpace(str) == "" {
				if f.Required {
					return loginJSONError(http.StatusBadRequest, "invalid_auth", "missing required field "+f.Name)
				}
				continue
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
	// The deterministic file name (derived from the key, plus any declared
	// FilenameFields) makes a repeated submission of the same identity
	// overwrite the same pending session instead of tripping the provider's
	// duplicate-key guard, while distinct identities sharing a key mint
	// distinct names instead of overwriting each other's ownership entry.
	fileName, err := loginFileName(l.provider, key, l.filenameFields, candidate)
	if err != nil {
		return loginJSONError(http.StatusBadRequest, "invalid_auth", "cannot encode auth storage")
	}
	path := fileName
	if s.authDir != "" {
		path = strings.TrimRight(s.authDir, "/") + "/" + fileName
	}
	// The validation parse is provisional: no auth file exists yet, and the
	// login may still be abandoned before the host saves one. Mark the
	// context with the session expiry so ownership guards (KeyOwnership or
	// a provider's own) check for duplicates without committing filename
	// ownership the abandoned login would never release.
	out, err := l.parse(withProvisionalParse(ctx, s.expiresAt), pluginapi.AuthParseRequest{Provider: l.provider, FileName: fileName, Path: path, RawJSON: raw})
	if err != nil {
		mapped := MapError(err)
		status := mapped.HTTPStatus
		if status < 400 || status > 599 {
			status = http.StatusBadRequest
		}
		// Providers may echo a secret URL-encoded or JSON-escaped; scrub
		// those recoverable forms too. The recipient already holds the
		// secrets (they just typed them), so this keeps key material out
		// of host and middleware response logs.
		message := loginScrubSecret(mapped.Message, key)
		for _, f := range l.fields {
			if !f.Secret {
				continue
			}
			if v, ok := candidate[f.Name].(string); ok {
				message = loginScrubSecret(message, v)
			}
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
	case current.consumed:
		// The host already collected this login: a second POST cannot be
		// delivered (the poller gets pending until expiry), so fail loudly
		// instead of storing a key the operator thinks is live.
		l.mu.Unlock()
		return loginJSONError(http.StatusConflict, "login_completed", "this login was already completed; start a new login")
	default:
		s.auth, s.auths, s.done = auth, out.Auths, true
	}
	l.mu.Unlock()
	return loginJSON(http.StatusOK, map[string]any{"ok": true})
}
