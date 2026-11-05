// Ownership is an auth-only fixture for the real pinned host loader.
package main

import (
	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func main() {}
func init() {
	if err := cabi.Export(makePlugin); err != nil {
		panic("cannot install ownership fixture")
	}
}

func makePlugin(host providerkit.Caller) (*providerkit.Runtime, error) {
	return providerkit.New(providerkit.Definition{
		Metadata: pluginapi.Metadata{Name: "ownership", Version: "0.1.0", Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-kit"},
		Capabilities: pluginapi.Capabilities{
			AuthProvider: ownershipAuth{providerkit.APIKeyAuth{Provider: "ownership-portal"}},
		},
	}, host)
}

type ownershipAuth struct{ providerkit.APIKeyAuth }

func (ownershipAuth) CredentialProviders() []string {
	return []string{" legacy-echo ", "", "legacy-echo"}
}
