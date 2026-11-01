package providerkit

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	loginResourceBase = "/v0/resource/plugins/echo-id"
	loginSubmitPath   = "/v0/management/plugin-login/echo"
	loginBaseURL      = "http://127.0.0.1:19000/v0/management/oauth-callback"
)

func echoLogin(t *testing.T) *APIKeyLogin {
	t.Helper()
	auth := APIKeyAuth{Provider: "echo"}
	login, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "echo", Parse: auth.ParseAuth})
	if err != nil {
		t.Fatal(err)
	}
	return login
}

func loginRuntime(t *testing.T, login *APIKeyLogin) *Runtime {
	t.Helper()
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{
		AuthProvider: APIKeyAuth{Provider: "echo", Login: login}, ManagementAPI: login}}, nil)
	register(t, r)
	env := rpc(t, r, "management.register", pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: loginResourceBase})
	if !env.OK {
		t.Fatalf("register management: %+v", env.Error)
	}
	return r
}

func startLogin(t *testing.T, r *Runtime) pluginapi.AuthLoginStartResponse {
	t.Helper()
	env := rpc(t, r, "auth.login.start", pluginapi.AuthLoginStartRequest{Provider: "echo", BaseURL: loginBaseURL, Host: pluginapi.HostConfigSummary{AuthDir: "/tmp/echo-auth"}})
	if !env.OK {
		t.Fatalf("start: %+v", env.Error)
	}
	var resp pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func manage(t *testing.T, r *Runtime, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	t.Helper()
	env := rpc(t, r, "management.handle", req)
	if !env.OK {
		t.Fatalf("handle: %+v", env.Error)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLoginConfigValidation(t *testing.T) {
	echoAuth := APIKeyAuth{Provider: "echo"}
	parse := echoAuth.ParseAuth
	for name, cfg := range map[string]APIKeyLoginConfig{
		"blank provider":     {Provider: " ", Parse: parse},
		"path traversal":     {Provider: "a/../b", Parse: parse},
		"slash":              {Provider: "a/b", Parse: parse},
		"shell provider":     {Provider: "a$b`c", Parse: parse},
		"quoted provider":    {Provider: `a'b"c`, Parse: parse},
		"leading dash":       {Provider: "-echo", Parse: parse},
		"nil parse":          {Provider: "echo"},
		"blank field":        {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{}}},
		"shell field":        {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "a$b"}}},
		"backtick field":     {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "a`b"}}},
		"dashed field":       {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "a-b"}}},
		"dotted field":       {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "a.b"}}},
		"spaced field":       {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "a b"}}},
		"reserved field":     {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "api_key"}}},
		"host-managed field": {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "weight"}}},
		"duplicate field":    {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "x"}, {Name: "x"}}},
		"reserved id":        {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "id"}}},
		"shell collision":    {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "Region"}, {Name: "region"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAPIKeyLogin(cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	if _, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "echo", Parse: parse,
		Fields: []APIKeyLoginField{{Name: "region", Label: "Region", Required: true}}}); err != nil {
		t.Fatal(err)
	}
	// Dashes are the only punctuation a real provider key needs
	// (cloudflare-workers-ai, nvidia-nim, zai-coding-plan, opencode-go).
	if _, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "cloudflare-workers-ai", Parse: parse,
		Fields: []APIKeyLoginField{{Name: "account_id"}, {Name: "daily_free_neurons2"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestLoginRegistrationAdvertisesManagement(t *testing.T) {
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{
		AuthProvider: APIKeyAuth{Provider: "echo", Login: echoLogin(t)}, ManagementAPI: echoLogin(t)}}, nil)
	env := rpc(t, r, "plugin.register", map[string]any{"schema_version": 10})
	if !env.OK {
		t.Fatal(env.Error)
	}
	var reg struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.Capabilities["management_api"] != true {
		t.Fatalf("capabilities %v", reg.Capabilities)
	}
}

func TestLoginStartRequiresRegistration(t *testing.T) {
	login := echoLogin(t)
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{
		AuthProvider: APIKeyAuth{Provider: "echo", Login: login}, ManagementAPI: login}}, nil)
	register(t, r)
	// Routes were never registered, so no page URL can be built.
	if env := rpc(t, r, "auth.login.start", pluginapi.AuthLoginStartRequest{BaseURL: loginBaseURL}); env.OK || env.Error.Code != "login_unavailable" {
		t.Fatalf("unregistered start: %+v", env)
	}
	// The page URL is root-relative, so StartLogin no longer depends on the
	// host callback base URL: any value (including empty) works once routes
	// are registered.
	registered := loginRuntime(t, login)
	for _, base := range []string{"", "not-a-url", "ftp://host/x", "http://", loginBaseURL} {
		env := rpc(t, registered, "auth.login.start", pluginapi.AuthLoginStartRequest{BaseURL: base})
		if !env.OK {
			t.Fatalf("base %q: %+v", base, env)
		}
		var resp pluginapi.AuthLoginStartResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(resp.URL, loginResourceBase+"/login?state=") {
			t.Fatalf("base %q: page url %q", base, resp.URL)
		}
	}
}

// TestLoginRejectsEmptyResourceBase pins the only remaining StartLogin
// failure mode: registration without a resource base (routes never
// registered) keeps failing with login_unavailable.
func TestLoginRejectsEmptyResourceBase(t *testing.T) {
	login := echoLogin(t)
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{
		AuthProvider: APIKeyAuth{Provider: "echo", Login: login}, ManagementAPI: login}}, nil)
	register(t, r)
	env := rpc(t, r, "management.register", pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: ""})
	if !env.OK {
		t.Fatalf("register management: %+v", env.Error)
	}
	if env := rpc(t, r, "auth.login.start", pluginapi.AuthLoginStartRequest{BaseURL: loginBaseURL}); env.OK || env.Error.Code != "login_unavailable" {
		t.Fatalf("empty resource base start: %+v", env)
	}
}

// TestLoginNormalizesBareRouteBases proves bases without a leading slash
// still yield root-relative page URLs and a management-base submit block: a
// bare "v0/..." URL would resolve against the panel's route path instead of
// its origin. It also pins the two-base split (page served from the resource
// base, key posted to the management base) and distinct consecutive states.
func TestLoginNormalizesBareRouteBases(t *testing.T) {
	login := echoLogin(t)
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{
		AuthProvider: APIKeyAuth{Provider: "echo", Login: login}, ManagementAPI: login}}, nil)
	register(t, r)
	env := rpc(t, r, "management.register", pluginapi.ManagementRegistrationRequest{
		BasePath: "v0/management", ResourceBasePath: "v0/resource/plugins/echo-id"})
	if !env.OK {
		t.Fatalf("register management: %+v", env.Error)
	}
	var first pluginapi.AuthLoginStartResponse
	for i := 0; i < 2; i++ {
		env := rpc(t, r, "auth.login.start", pluginapi.AuthLoginStartRequest{BaseURL: loginBaseURL})
		if !env.OK {
			t.Fatalf("start: %+v", env)
		}
		var resp pluginapi.AuthLoginStartResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(resp.URL, "/v0/resource/plugins/echo-id/login?state=") || strings.Contains(resp.URL, "://") {
			t.Fatalf("page url %q", resp.URL)
		}
		if i == 0 {
			first = resp
		} else if resp.State == first.State || resp.URL == first.URL {
			t.Fatalf("states not distinct: %+v %+v", first, resp)
		}
	}
	page := manage(t, r, pluginapi.ManagementRequest{Method: "GET",
		Path: "/v0/resource/plugins/echo-id/login", Query: map[string][]string{"state": {first.State}}})
	// Quotes are HTML-escaped in the raw body; the path itself is not.
	if page.StatusCode != 200 || !strings.Contains(string(page.Body), "$CLIPROXY_BASE/v0/management/plugin-login/echo") {
		t.Fatalf("page posts to the wrong base: %+v", page.StatusCode)
	}
}

// TestRouteBaseRejectsHostile pins the shell-safety guard: the base lands
// inside the operator's pasted shell block in double quotes, so shell
// metacharacters are rejected to "" (StartLogin then fails login_unavailable
// instead of serving a hostile block), as is a protocol-relative "//" base
// that would resolve off-origin in the panel.
func TestRouteBaseRejectsHostile(t *testing.T) {
	for _, base := range []string{
		`/v0/management"$(evil)"`,
		"/v0/management`evil`",
		"/v0/$management",
		`/v0\management`,
		"/v0/management\n松",
		"/v0/management\revil",
		"//evil.com/x",
		"//",
	} {
		if got := routeBase(base); got != "" {
			t.Errorf("routeBase(%q) = %q, want empty", base, got)
		}
	}
	for base, want := range map[string]string{
		"":                "",
		"/":               "",
		"v0/management":   "/v0/management",
		"/v0/management/": "/v0/management",
		"  /v0/m  ":       "/v0/m",
	} {
		if got := routeBase(base); got != want {
			t.Errorf("routeBase(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestLoginFullFlow(t *testing.T) {
	r := loginRuntime(t, echoLogin(t))
	start := startLogin(t, r)
	// Root-relative on purpose: absolutizing with the host's loopback base
	// would hand the panel a link a remote browser cannot load.
	if !strings.HasPrefix(start.URL, loginResourceBase+"/login?state=") {
		t.Fatalf("page url %q", start.URL)
	}
	if strings.Contains(start.URL, "://") {
		t.Fatalf("page url must be relative, got %q", start.URL)
	}
	if len(start.State) != 32 || strings.Trim(start.State, "0123456789abcdef") != "" {
		t.Fatalf("state %q", start.State)
	}
	if !strings.Contains(start.URL, "state="+start.State) {
		t.Fatalf("url/state mismatch %q %q", start.URL, start.State)
	}

	page := manage(t, r, pluginapi.ManagementRequest{Method: "GET",
		Path: loginResourceBase + "/login", Query: map[string][]string{"state": {start.State}}})
	if page.StatusCode != 200 || !strings.Contains(page.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("page %+v", page)
	}
	body := string(page.Body)
	if !strings.Contains(body, start.State) || !strings.Contains(body, "curl -X POST") || !strings.Contains(body, "api_key") {
		t.Fatalf("page missing instructions: %s", body)
	}
	// The page mirrors the panel's login steps and steers operators away
	// from the panel's OAuth-only callback box.
	for _, want := range []string{"Return to the management UI", "callback-URL box is only for OAuth providers"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q: %s", want, body)
		}
	}

	poll := func() pluginapi.AuthLoginPollResponse {
		env := rpc(t, r, "auth.login.poll", pluginapi.AuthLoginPollRequest{Provider: "echo", State: start.State})
		if !env.OK {
			t.Fatalf("poll: %+v", env.Error)
		}
		var resp pluginapi.AuthLoginPollResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if got := poll(); got.Status != pluginapi.AuthLoginStatusPending || !strings.Contains(got.Message, "completes automatically") {
		t.Fatalf("pending %+v", got)
	}

	submit := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Headers: map[string][]string{"Authorization": {"Bearer management-secret"}},
		Body:    []byte(`{"state":"` + start.State + `","api_key":"example-key","label":"Example"}`)})
	if submit.StatusCode != 200 || string(submit.Body) != `{"ok":true}` {
		t.Fatalf("submit %+v", submit)
	}

	got := poll()
	if got.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("success %+v", got)
	}
	if got.Auth.ID != "echo:c018c41c1afa" || got.Auth.FileName != "echo-c018c41c1afa.json" || got.Auth.Label != "Example" {
		t.Fatalf("auth %+v", got.Auth)
	}
	if key, err := APIKey(got.Auth.StorageJSON, nil); err != nil || key != "example-key" {
		t.Fatalf("stored key %q %v", key, err)
	}
	// Single delivery: a second poll waits credential-free instead of
	// replaying the credentials or failing the host login session.
	if again := poll(); again.Status != pluginapi.AuthLoginStatusPending || len(again.Auth.StorageJSON) != 0 {
		t.Fatalf("replay %+v", again)
	}
}

func TestLoginSubmitGuards(t *testing.T) {
	r := loginRuntime(t, echoLogin(t))
	start := startLogin(t, r)
	submit := func(body string) pluginapi.ManagementResponse {
		return manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath, Body: []byte(body)})
	}
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"not json":       {`{`, 400, "invalid_request"},
		"not object":     {`[]`, 400, "invalid_request"},
		"duplicate keys": {`{"state":"x","state":"y"}`, 400, "invalid_request"},
		"unknown state":  {`{"state":"missing","api_key":"example-key"}`, 400, "login_expired"},
		"blank key":      {`{"state":"` + start.State + `","api_key":" "}`, 400, "invalid_auth"},
		"missing key":    {`{"state":"` + start.State + `"}`, 400, "invalid_auth"},
		"unknown field":  {`{"state":"` + start.State + `","api_key":"example-key","api-key":"x"}`, 400, "invalid_request"},
		"bad label":      {`{"state":"` + start.State + `","api_key":"example-key","label":7}`, 400, "invalid_auth"},
		"oversize":       {`{"state":"` + start.State + `","api_key":"` + strings.Repeat("k", maxLoginSubmitBody) + `"}`, 400, "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := submit(tc.body)
			var decoded struct {
				Error *pluginabi.Error `json:"error"`
			}
			if err := json.Unmarshal(resp.Body, &decoded); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.status || decoded.Error == nil || decoded.Error.Code != tc.code {
				t.Fatalf("status %d body %s", resp.StatusCode, resp.Body)
			}
			if strings.Contains(string(resp.Body), "example-key") {
				t.Fatalf("response leaked key: %s", resp.Body)
			}
		})
	}
	if resp := manage(t, r, pluginapi.ManagementRequest{Method: "GET", Path: loginSubmitPath}); resp.StatusCode != 405 {
		t.Fatalf("submit GET: %+v", resp)
	}
	if resp := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugin-login/other"}); resp.StatusCode != 404 {
		t.Fatalf("other provider: %+v", resp)
	}
	page := manage(t, r, pluginapi.ManagementRequest{Method: "GET", Path: loginResourceBase + "/login",
		Query: map[string][]string{"state": {"no-such-session"}}})
	if page.StatusCode != 200 || !strings.Contains(string(page.Body), "expired") {
		t.Fatalf("expired page %+v", page)
	}
}

func TestLoginResubmitIsIdempotent(t *testing.T) {
	r := loginRuntime(t, echoLogin(t))
	complete := func() pluginapi.AuthLoginPollResponse {
		start := startLogin(t, r)
		resp := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
			Body: []byte(`{"state":"` + start.State + `","api_key":"example-key"}`)})
		if resp.StatusCode != 200 {
			t.Fatalf("submit %+v", resp)
		}
		env := rpc(t, r, "auth.login.poll", pluginapi.AuthLoginPollRequest{State: start.State})
		if !env.OK {
			t.Fatalf("poll: %+v", env.Error)
		}
		var out pluginapi.AuthLoginPollResponse
		if err := json.Unmarshal(env.Result, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first, second := complete(), complete()
	if first.Status != pluginapi.AuthLoginStatusSuccess || second.Status != pluginapi.AuthLoginStatusSuccess ||
		first.Auth.ID != second.Auth.ID || first.Auth.FileName != second.Auth.FileName {
		t.Fatalf("not idempotent: %+v %+v", first, second)
	}
}

func TestLoginExpiry(t *testing.T) {
	expiryAuth := APIKeyAuth{Provider: "echo"}
	login, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "echo",
		Parse: expiryAuth.ParseAuth, SessionTTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{
		AuthProvider: APIKeyAuth{Provider: "echo", Login: login}, ManagementAPI: login}}, nil)
	register(t, r)
	env := rpc(t, r, "management.register", pluginapi.ManagementRegistrationRequest{ResourceBasePath: loginResourceBase})
	if !env.OK {
		t.Fatal(env.Error)
	}
	start := startLogin(t, r)
	env = rpc(t, r, "auth.login.poll", pluginapi.AuthLoginPollRequest{State: start.State})
	if !env.OK {
		t.Fatalf("poll: %+v", env.Error)
	}
	var out pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != pluginapi.AuthLoginStatusError || !strings.Contains(out.Message, "start a new login") {
		t.Fatalf("expiry %+v", out)
	}
}

func TestLoginScrubsLeakyProviderErrors(t *testing.T) {
	leaky := func(_ context.Context, _ pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
		return pluginapi.AuthParseResponse{}, &pluginabi.Error{Code: "upstream_says_no", Message: "upstream rejected example-key as invalid", HTTPStatus: 400}
	}
	login, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "echo", Parse: leaky})
	if err != nil {
		t.Fatal(err)
	}
	r := loginRuntime(t, login)
	start := startLogin(t, r)
	resp := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key"}`)})
	if resp.StatusCode != 400 || strings.Contains(string(resp.Body), "example-key") || !strings.Contains(string(resp.Body), "[redacted]") {
		t.Fatalf("scrub %+v", resp)
	}
}

// TestLoginScrubsEncodedKeyEchoes proves the scrub also catches recoverable
// forms of the key: a provider echoing the URL-encoded or JSON-escaped key
// must not leak it into the operator-facing error (and host logs).
func TestLoginScrubsEncodedKeyEchoes(t *testing.T) {
	const key = `a"b c/d`
	leaky := func(_ context.Context, _ pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
		return pluginapi.AuthParseResponse{}, &pluginabi.Error{Code: "upstream_says_no",
			Message: `upstream rejected a%22b+c%2Fd and a\"b c/d as invalid`, HTTPStatus: 400}
	}
	login, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "echo", Parse: leaky})
	if err != nil {
		t.Fatal(err)
	}
	r := loginRuntime(t, login)
	start := startLogin(t, r)
	resp := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"` + loginJSONString(key) + `"}`)})
	body := string(resp.Body)
	if resp.StatusCode != 400 || strings.Contains(body, key) ||
		strings.Contains(body, "a%22b+c%2Fd") || strings.Contains(body, `a\"b c/d`) ||
		strings.Count(body, "[redacted]") != 2 {
		t.Fatalf("encoded scrub %+v", resp)
	}
}

func TestLoginRequiredFields(t *testing.T) {
	regionAuth := APIKeyAuth{Provider: "echo", Validate: func(fields map[string]any) error {
		if fields["region"] == nil {
			return &pluginabi.Error{Code: "region_required", Message: "region is required", HTTPStatus: 400}
		}
		return nil
	}}
	login, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "echo", Parse: regionAuth.ParseAuth,
		Fields: []APIKeyLoginField{{Name: "region", Label: "Region", Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	r := loginRuntime(t, login)
	start := startLogin(t, r)
	missing := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key"}`)})
	if missing.StatusCode != 400 || !strings.Contains(string(missing.Body), "region") {
		t.Fatalf("required %+v", missing)
	}
	ok := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key","region":"eu"}`)})
	if ok.StatusCode != 200 {
		t.Fatalf("with region %+v", ok)
	}
	env := rpc(t, r, "auth.login.poll", pluginapi.AuthLoginPollRequest{State: start.State})
	if !env.OK {
		t.Fatalf("poll: %+v", env.Error)
	}
	var out pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll %+v", out)
	}
	if key, err := APIKey(out.Auth.StorageJSON, nil); err != nil || key != "example-key" {
		t.Fatalf("stored %q %v", key, err)
	}
	var store map[string]any
	if err := json.Unmarshal(out.Auth.StorageJSON, &store); err != nil || store["region"] != "eu" {
		t.Fatalf("extra field lost: %s", out.Auth.StorageJSON)
	}
}

func TestLoginNilHelperKeepsAPIKeyOnly(t *testing.T) {
	var login *APIKeyLogin
	if _, err := login.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{}); err == nil {
		t.Fatal("nil login started")
	}
	if _, err := login.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{}); err == nil {
		t.Fatal("nil login polled")
	}
	if _, err := login.RegisterManagement(context.Background(), pluginapi.ManagementRegistrationRequest{}); err == nil {
		t.Fatal("nil login registered")
	}
	if _, err := login.HandleManagement(context.Background(), pluginapi.ManagementRequest{}); err == nil {
		t.Fatal("nil login handled")
	}
}

func TestLoginRegistrationWireRoundTrip(t *testing.T) {
	login := echoLogin(t)
	resp, err := login.RegisterManagement(context.Background(), pluginapi.ManagementRegistrationRequest{
		BasePath: "/v0/management", ResourceBasePath: loginResourceBase})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror of the host rpcManagementRegistrationResponse: decoding must
	// succeed or the host registers no routes and the login is dead.
	var wire struct {
		Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
		Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("host cannot decode registration: %v", err)
	}
	if len(wire.Routes) != 1 || len(wire.Resources) != 1 {
		t.Fatalf("routes lost on the wire: %+v", wire)
	}
}

func TestLoginSubmitAfterExpiryIsRejected(t *testing.T) {
	expiryAuth := APIKeyAuth{Provider: "echo"}
	slow := func(ctx context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
		time.Sleep(120 * time.Millisecond)
		return expiryAuth.ParseAuth(ctx, req)
	}
	login, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "echo",
		Parse: slow, SessionTTL: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r := loginRuntime(t, login)
	start := startLogin(t, r)
	resp := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key"}`)})
	if resp.StatusCode != 400 || !strings.Contains(string(resp.Body), "login_expired") {
		t.Fatalf("expired submit reported success: %+v", resp)
	}
}

func TestLoginConcurrentPollSingleDelivery(t *testing.T) {
	r := loginRuntime(t, echoLogin(t))
	start := startLogin(t, r)
	if resp := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key"}`)}); resp.StatusCode != 200 {
		t.Fatalf("submit %+v", resp)
	}
	poll := func() pluginapi.AuthLoginPollResponse {
		env := rpc(t, r, "auth.login.poll", pluginapi.AuthLoginPollRequest{State: start.State})
		if !env.OK {
			t.Fatalf("poll: %+v", env.Error)
		}
		var out pluginapi.AuthLoginPollResponse
		if err := json.Unmarshal(env.Result, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first, second := poll(), poll()
	var success, pending int
	for _, out := range []pluginapi.AuthLoginPollResponse{first, second} {
		switch out.Status {
		case pluginapi.AuthLoginStatusSuccess:
			success++
			if len(out.Auth.StorageJSON) == 0 {
				t.Fatalf("success without credentials: %+v", out)
			}
		case pluginapi.AuthLoginStatusPending:
			pending++
		default:
			t.Fatalf("concurrent poll failed the login: %+v", out)
		}
	}
	if success != 1 || pending != 1 {
		t.Fatalf("want exactly one delivery and one wait, got %+v %+v", first, second)
	}
}

func TestLoginPageKeepsSecretsOutOfCommands(t *testing.T) {
	r := loginRuntime(t, echoLogin(t))
	start := startLogin(t, r)
	page := manage(t, r, pluginapi.ManagementRequest{Method: "GET", Path: loginResourceBase + "/login",
		Query: map[string][]string{"state": {start.State}}})
	if page.StatusCode != 200 {
		t.Fatalf("page %+v", page)
	}
	// The command is HTML-escaped inside <pre>: check the pasted text.
	body := html.UnescapeString(string(page.Body))
	if !strings.Contains(body, "read -rs") || !strings.Contains(body, "$CLIPROXY_BASE") {
		t.Fatalf("page missing hidden-input instructions: %s", body)
	}
	if strings.Contains(body, "YOUR_API_KEY") {
		t.Fatalf("page trains operators to paste secrets into commands: %s", body)
	}
	// Secrets travel over stdin only: the bearer header in a
	// process-substitution file, the JSON body in a heredoc. Neither may
	// appear as a curl argument, where ps and /proc could see it.
	for _, want := range []string{"-H @<(", "--data-binary @-", "<<EOF", "unset CLIPROXY_MANAGEMENT_KEY"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing stdin secret transport %q: %s", want, body)
		}
	}
	for _, banned := range []string{`-d "$(printf`, `-H "Authorization: Bearer $`, "read -s -p"} {
		if strings.Contains(body, banned) {
			t.Fatalf("page leaks secrets into process arguments or uses bash-only read flags %q: %s", banned, body)
		}
	}
	// Prompts stay portable: printf carries the prompt (zsh rejects read -p)
	// and values are shell-escaped with builtins before reaching curl.
	for _, want := range []string{"printf 'Management key: '; read -rs", "read -rs PLUGIN_API_KEY", "${PLUGIN_API_KEY//\\\\/\\\\\\\\}"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing portable prompt or builtin escaping %q: %s", want, body)
		}
	}
	// Remote operators must reach for https first so both secrets are
	// encrypted in transit; plain http stays a loopback-only option.
	if !strings.Contains(body, "https://host:port") || !strings.Contains(body, "always use https://") {
		t.Fatalf("page missing https-first CLIPROXY_BASE guidance: %s", body)
	}
}

func TestLoginPageIncludesRequiredFields(t *testing.T) {
	echoAuth := APIKeyAuth{Provider: "echo"}
	login, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "echo", Parse: echoAuth.ParseAuth,
		Fields: []APIKeyLoginField{
			{Name: "account_id", Label: "Account ID", Description: "Cloudflare account", Required: true},
			{Name: "note", Label: "Note"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	r := loginRuntime(t, login)
	start := startLogin(t, r)
	page := manage(t, r, pluginapi.ManagementRequest{Method: "GET", Path: loginResourceBase + "/login",
		Query: map[string][]string{"state": {start.State}}})
	if page.StatusCode != 200 {
		t.Fatalf("page %+v", page)
	}
	// The command is HTML-escaped inside <pre>: check the pasted text.
	body := html.UnescapeString(string(page.Body))
	// Every declared field is prompted for and lands in the JSON body, so a
	// verbatim run cannot fail with "missing required field".
	for _, want := range []string{
		"read -r PLUGIN_FIELD_ACCOUNT_ID",
		"read -r PLUGIN_FIELD_NOTE",
		`"account_id":"$ESC_FIELD_ACCOUNT_ID"`,
		`"note":"$ESC_FIELD_NOTE"`,
		"Account ID (required)",
		"Note (optional)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing required-field wiring %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Optional provider fields") {
		t.Fatalf("page mislabels a section with required fields as optional: %s", body)
	}
	// A blank required answer is a missing field; a blank optional is skipped.
	blankRequired := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key","account_id":" "}`)})
	if blankRequired.StatusCode != 400 || !strings.Contains(string(blankRequired.Body), "missing required field") {
		t.Fatalf("blank required accepted: %+v", blankRequired)
	}
	blankOptional := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key","account_id":"ac-1","note":""}`)})
	if blankOptional.StatusCode != 200 {
		t.Fatalf("blank optional rejected: %+v", blankOptional)
	}
	env := rpc(t, r, "auth.login.poll", pluginapi.AuthLoginPollRequest{State: start.State})
	if !env.OK {
		t.Fatalf("poll: %+v", env.Error)
	}
	var out pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll %+v", out)
	}
	var store map[string]any
	if err := json.Unmarshal(out.Auth.StorageJSON, &store); err != nil {
		t.Fatal(err)
	}
	if store["account_id"] != "ac-1" {
		t.Fatalf("required field lost: %s", out.Auth.StorageJSON)
	}
	if _, ok := store["note"]; ok {
		t.Fatalf("blank optional stored as empty: %s", out.Auth.StorageJSON)
	}
}

func TestLoginResubmitAfterConsumedIsRejected(t *testing.T) {
	r := loginRuntime(t, echoLogin(t))
	start := startLogin(t, r)
	if resp := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"example-key"}`)}); resp.StatusCode != 200 {
		t.Fatalf("submit %+v", resp)
	}
	env := rpc(t, r, "auth.login.poll", pluginapi.AuthLoginPollRequest{State: start.State})
	if !env.OK {
		t.Fatalf("poll: %+v", env.Error)
	}
	var out pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll %+v", out)
	}
	// The login was already collected: a second POST must fail loudly instead
	// of storing a key the operator believes is live.
	again := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath,
		Body: []byte(`{"state":"` + start.State + `","api_key":"other-key"}`)})
	if again.StatusCode != http.StatusConflict || !strings.Contains(string(again.Body), "login_completed") {
		t.Fatalf("consumed resubmit not rejected: %+v", again)
	}
}

func TestLoginPageBlockSurvivesLineByLinePaste(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	echoAuth := APIKeyAuth{Provider: "echo"}
	login, err := NewAPIKeyLogin(APIKeyLoginConfig{Provider: "echo", Parse: echoAuth.ParseAuth,
		Fields: []APIKeyLoginField{{Name: "account_id", Label: "Account ID", Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	r := loginRuntime(t, login)
	start := startLogin(t, r)
	page := manage(t, r, pluginapi.ManagementRequest{Method: "GET", Path: loginResourceBase + "/login",
		Query: map[string][]string{"state": {start.State}}})
	if page.StatusCode != 200 {
		t.Fatalf("page %+v", page)
	}
	body := html.UnescapeString(string(page.Body))
	pre, post := strings.Index(body, "<pre>"), strings.Index(body, "</pre>")
	if pre < 0 || post < 0 || post <= pre {
		t.Fatalf("page has no pasted block: %s", body)
	}
	block := body[pre+len("<pre>") : post]
	lines := strings.Split(strings.TrimSpace(block), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "{" || strings.TrimSpace(lines[len(lines)-1]) != "}" {
		t.Fatalf("pasted block is not brace-grouped, so a line-by-line paste feeds script lines to read:\n%s", block)
	}
	// Nasty but exact: quotes, backslashes, tab, $ and backticks must
	// survive the builtin escaping and the unquoted heredoc verbatim.
	mgmtKey := "mgmt-test-key"
	apiKey := "ab\"cd\\ef$HOME`tick`\tplain"
	account := "ac-1"
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath, gotAuth = req.URL.Path, req.Header.Get("Authorization")
		raw, _ := io.ReadAll(req.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	// Paste simulation: the block and the typed secrets share one stdin
	// stream, the way a bracketed-paste-off terminal delivers them. Without
	// the braces bash runs the first line at once and read -rs eats the
	// next pasted line; with them bash parses through the closing brace
	// before any read runs, so the reads consume the secrets that follow.
	cmd := exec.Command("bash")
	cmd.Env = append(os.Environ(), "CLIPROXY_BASE="+srv.URL)
	cmd.Stdin = strings.NewReader(block + mgmtKey + "\n" + apiKey + "\n" + account + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pasted block failed: %v\n%s", err, out)
	}
	if gotPath != "/v0/management/plugin-login/echo" {
		t.Fatalf("wrong submit path %q output:\n%s", gotPath, out)
	}
	if gotAuth != "Bearer "+mgmtKey {
		t.Fatalf("management key mangled: %q output:\n%s", gotAuth, out)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(gotBody), &decoded); err != nil {
		t.Fatalf("body is not valid JSON %q: %v\n%s", gotBody, err, out)
	}
	if decoded["state"] != start.State || decoded["api_key"] != apiKey || decoded["account_id"] != account {
		t.Fatalf("secrets mangled: %s", gotBody)
	}
}

func TestLoginSpecialCharacterKey(t *testing.T) {
	r := loginRuntime(t, echoLogin(t))
	start := startLogin(t, r)
	// A key with JSON-significant characters must survive the round trip: the
	// page shell-escapes it with builtins, and the endpoint stores it verbatim.
	key := `ab"cd\ef`
	submit, err := json.Marshal(map[string]any{"state": start.State, "api_key": key})
	if err != nil {
		t.Fatal(err)
	}
	if resp := manage(t, r, pluginapi.ManagementRequest{Method: "POST", Path: loginSubmitPath, Body: submit}); resp.StatusCode != 200 {
		t.Fatalf("special-char submit %+v", resp)
	}
	env := rpc(t, r, "auth.login.poll", pluginapi.AuthLoginPollRequest{State: start.State})
	if !env.OK {
		t.Fatalf("poll: %+v", env.Error)
	}
	var out pluginapi.AuthLoginPollResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll %+v", out)
	}
	if got, err := APIKey(out.Auth.StorageJSON, nil); err != nil || got != key {
		t.Fatalf("stored %q want %q (err %v)", got, key, err)
	}
}
