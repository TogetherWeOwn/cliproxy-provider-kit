// Echo is a mock-upstream-only provider example, not a production integration.
package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func main() {}
func init() {
	if err := cabi.Export(makePlugin); err != nil {
		panic("cannot install echo factory")
	}
}
func makePlugin(host providerkit.Caller) (*providerkit.Runtime, error) {
	var upstream atomic.Value
	upstream.Store("")
	configure := func(_ context.Context, raw []byte) error {
		var cfg struct {
			URL string `yaml:"upstream_url"`
		}
		if yaml.Unmarshal(raw, &cfg) != nil {
			return &pluginabi.Error{Code: "invalid_config", Message: "invalid echo configuration", HTTPStatus: 400}
		}
		u, err := url.Parse(cfg.URL)
		if err != nil || u == nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return &pluginabi.Error{Code: "invalid_config", Message: "echo requires a loopback upstream_url", HTTPStatus: 400}
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return &pluginabi.Error{Code: "invalid_config", Message: "echo only connects to literal loopback addresses", HTTPStatus: 400}
		}
		upstream.Store(cfg.URL)
		return nil
	}
	executor := providerkit.Passthrough{Provider: "echo", BuildRequest: func(_ context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
		key, err := providerkit.APIKey(r.StorageJSON, r.AuthMetadata)
		if err != nil {
			return pluginapi.HTTPRequest{}, err
		}
		return pluginapi.HTTPRequest{Method: http.MethodPost, URL: upstream.Load().(string), Headers: http.Header{"Authorization": {"Bearer " + key}, "Content-Type": {"application/json"}}, Body: r.Payload}, nil
	}, PrepareHTTP: func(_ context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error) {
		key, err := providerkit.APIKey(r.StorageJSON, r.Metadata)
		if err != nil {
			return pluginapi.HTTPRequest{}, err
		}
		// The example does not trust arbitrary HttpRequest URLs with credentials.
		headers := r.Headers.Clone()
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set("Authorization", "Bearer "+key)
		return pluginapi.HTTPRequest{Method: r.Method, URL: upstream.Load().(string), Headers: headers, Body: r.Body}, nil
	}}
	quota := providerkit.NewCachedQuota("echo", "Echo fixture", func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "Example credits", Buckets: []pluginapi.QuotaBucket{{Window: "weekly", RemainingFraction: 1, Description: "Fixture data, not a real subscription"}}}}}, nil
	})
	return providerkit.New(providerkit.Definition{Metadata: pluginapi.Metadata{Name: "echo", Version: "0.1.0", Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-kit"}, ModelNamespace: "echo", Configure: configure, Capabilities: pluginapi.Capabilities{
		AuthProvider: providerkit.APIKeyAuth{Provider: "echo"}, ModelRegistrar: catalog{}, ModelProvider: catalog{}, Executor: executor, ExecutorModelScope: pluginapi.ExecutorModelScopeBoth, ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, QuotaProvider: quota,
	}}, host)
}

type catalog struct{}

func echoModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{{ID: "echo/echo-model", Object: "model", OwnedBy: "echo", DisplayName: "Echo fixture", ContextLength: 8192, MaxCompletionTokens: 1024}}
}
func (catalog) RegisterModels(context.Context, pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return pluginapi.ModelRegistrationResponse{Provider: "echo", Models: echoModels()}, nil
}
func (catalog) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: "echo", Models: echoModels()}, nil
}
func (catalog) ModelsForAuth(context.Context, pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: "echo", Models: echoModels()}, nil
}
