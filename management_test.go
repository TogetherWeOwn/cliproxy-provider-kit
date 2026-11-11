package providerkit

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type managementRegistrationOnly struct{}

func (managementRegistrationOnly) RegisterManagement(context.Context, pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	return pluginapi.ManagementRegistrationResponse{Routes: []pluginapi.ManagementRoute{{Method: "GET", Path: "status"}}}, nil
}

type managementFixture struct {
	response pluginapi.ManagementRegistrationResponse
}

func (m *managementFixture) RegisterManagement(context.Context, pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	return m.response, nil
}
func (*managementFixture) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return pluginapi.ManagementResponse{StatusCode: 200, Body: []byte(req.Path)}, nil
}

func TestNewRejectsManagementWithoutDispatcher(t *testing.T) {
	r, err := New(Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{ManagementAPI: managementRegistrationOnly{}}}, nil)
	if r != nil {
		r.Shutdown(context.Background())
	}
	if err == nil {
		t.Fatal("management registration accepted without an RPC dispatcher")
	}
}

func TestManagementRegistrationWireExcludesLocalHandlers(t *testing.T) {
	m := &managementFixture{}
	m.response = pluginapi.ManagementRegistrationResponse{
		Routes:    []pluginapi.ManagementRoute{{Method: "GET", Path: "status", Menu: "Status", Description: "Provider status", Handler: m}},
		Resources: []pluginapi.ResourceRoute{{Path: "help", Menu: "Help", Description: "Provider help", Handler: m}},
	}
	r := newRuntime(t, Definition{Metadata: metadata(), Capabilities: pluginapi.Capabilities{ManagementAPI: m}}, nil)
	register(t, r)
	env := rpc(t, r, "management.register", pluginapi.ManagementRegistrationRequest{})
	if !env.OK {
		t.Fatal(env.Error)
	}
	// v8.0.63 rpcManagementRegistrationResponse embeds these SDK route
	// types. Its decoder starts with nil ManagementHandler interfaces.
	var decoded pluginapi.ManagementRegistrationResponse
	if err := json.Unmarshal(env.Result, &decoded); err != nil {
		t.Fatalf("host cannot decode registration: %v", err)
	}
	want := pluginapi.ManagementRegistrationResponse{
		Routes:    []pluginapi.ManagementRoute{{Method: "GET", Path: "status", Menu: "Status", Description: "Provider status"}},
		Resources: []pluginapi.ResourceRoute{{Path: "help", Menu: "Help", Description: "Provider help"}},
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("registration changed metadata: %+v", decoded)
	}
	if m.response.Routes[0].Handler != m || m.response.Resources[0].Handler != m {
		t.Fatal("registration mutated provider-owned cached slices")
	}
	for _, path := range []string{"status", "help"} {
		env = rpc(t, r, "management.handle", pluginapi.ManagementRequest{Method: "GET", Path: path})
		var resp pluginapi.ManagementResponse
		if !env.OK || json.Unmarshal(env.Result, &resp) != nil || string(resp.Body) != path {
			t.Fatalf("RPC dispatch failed: %+v", env)
		}
	}
}
