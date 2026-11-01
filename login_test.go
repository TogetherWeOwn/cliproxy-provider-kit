package providerkit

import (
	"context"
	"encoding/json"
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
		"blank provider":  {Provider: " ", Parse: parse},
		"path traversal":  {Provider: "a/../b", Parse: parse},
		"slash":           {Provider: "a/b", Parse: parse},
		"nil parse":       {Provider: "echo"},
		"blank field":     {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{}}},
		"reserved field":  {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "api_key"}}},
		"duplicate field": {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "x"}, {Name: "x"}}},
		"reserved id":     {Provider: "echo", Parse: parse, Fields: []APIKeyLoginField{{Name: "id"}}},
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

func TestLoginStartRequiresRegistrationAndBaseURL(t *testing.T) {
	login := echoLogin(t)
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{
		AuthProvider: APIKeyAuth{Provider: "echo", Login: login}, ManagementAPI: login}}, nil)
	register(t, r)
	// Routes were never registered, so no page URL can be built.
	if env := rpc(t, r, "auth.login.start", pluginapi.AuthLoginStartRequest{BaseURL: loginBaseURL}); env.OK || env.Error.Code != "login_unavailable" {
		t.Fatalf("unregistered start: %+v", env)
	}
	registered := loginRuntime(t, login)
	for _, base := range []string{"", "not-a-url", "ftp://host/x", "http://"} {
		if env := rpc(t, registered, "auth.login.start", pluginapi.AuthLoginStartRequest{BaseURL: base}); env.OK || env.Error.Code != "login_unavailable" {
			t.Fatalf("base %q: %+v", base, env)
		}
	}
}

func TestLoginFullFlow(t *testing.T) {
	r := loginRuntime(t, echoLogin(t))
	start := startLogin(t, r)
	if !strings.HasPrefix(start.URL, "http://127.0.0.1:19000"+loginResourceBase+"/login?state=") {
		t.Fatalf("page url %q", start.URL)
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
	if got := poll(); got.Status != pluginapi.AuthLoginStatusPending || !strings.Contains(got.Message, start.URL) {
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
	body := string(page.Body)
	if !strings.Contains(body, "read -s") || !strings.Contains(body, "$CLIPROXY_BASE") {
		t.Fatalf("page missing hidden-input instructions: %s", body)
	}
	if strings.Contains(body, "YOUR_API_KEY") {
		t.Fatalf("page trains operators to paste secrets into commands: %s", body)
	}
}
