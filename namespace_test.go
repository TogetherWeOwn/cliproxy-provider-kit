package providerkit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func namespacedDefinition(ns string, registrar pluginapi.ModelRegistrar, provider pluginapi.ModelProvider) Definition {
	return Definition{
		Metadata:       metadata(),
		ModelNamespace: ns,
		Capabilities:   pluginapi.Capabilities{ModelRegistrar: registrar, ModelProvider: provider},
	}
}

func TestNewRejectsMissingModelNamespace(t *testing.T) {
	registrar := catalogStub{}
	for name, ns := range map[string]string{
		"missing":       "",
		"whitespace":    "   ",
		"with slash":    "nvidia/",
		"inner slash":   "nvidia/nim",
		"with space":    "nvidia nim",
		"leading slash": "/nvidia",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(namespacedDefinition(ns, registrar, registrar), nil); err == nil {
				t.Fatalf("namespace %q accepted", ns)
			}
		})
	}
	for _, tc := range []struct {
		name string
		def  Definition
	}{
		{"valid", namespacedDefinition("nvidia", registrar, registrar)},
		{"dotted", namespacedDefinition("opencode-go", registrar, registrar)},
		{"no model capabilities needs none", Definition{Metadata: metadata()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.def, nil); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
}

type catalogStub struct {
	register pluginapi.ModelRegistrationResponse
	static   pluginapi.ModelResponse
	forAuth  pluginapi.ModelResponse
}

func (s catalogStub) RegisterModels(context.Context, pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return s.register, nil
}
func (s catalogStub) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return s.static, nil
}
func (s catalogStub) ModelsForAuth(context.Context, pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return s.forAuth, nil
}

func modelIDs(models []pluginapi.ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

func decodeResult(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// mixedCatalog returns a model list exercising every namespace rule for ns.
func mixedCatalog(ns string) []pluginapi.ModelInfo {
	mk := func(id string) pluginapi.ModelInfo {
		return pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: "test", DisplayName: id}
	}
	return []pluginapi.ModelInfo{
		mk(ns + "/good-model"),
		mk(ns + "/second/layer"),
		mk("bare-model"),
		mk("other-ns/good-model"),
		mk(ns + "/" + ns + "/doubled"),
		mk(ns + "/"),
	}
}

func TestRegistrationPathsEnforceNamespace(t *testing.T) {
	stub := catalogStub{}
	stub.register = pluginapi.ModelRegistrationResponse{
		Provider: "test",
		Models:   mixedCatalog("nvidia"),
		Registrations: []pluginapi.ProviderModels{
			{Provider: "test-second", Models: mixedCatalog("nvidia")},
		},
	}
	stub.static = pluginapi.ModelResponse{Provider: "test", Models: mixedCatalog("nvidia")}
	stub.forAuth = pluginapi.ModelResponse{Provider: "test", Models: mixedCatalog("nvidia")}
	r := newRuntime(t, namespacedDefinition("nvidia", stub, stub), nil)
	register(t, r)

	want := []string{"nvidia/good-model", "nvidia/second/layer"}

	var reg pluginapi.ModelRegistrationResponse
	if env := rpc(t, r, "model.register", map[string]any{}); !env.OK {
		t.Fatal(env.Error)
	} else {
		decodeResult(t, env.Result, &reg)
	}
	// Filtering must not mutate the provider's own lists.
	if got := modelIDs(stub.register.Models); len(got) != 6 {
		t.Fatalf("provider slice mutated: %q", got)
	}
	if got := modelIDs(reg.Models); !equalStrings(got, want) {
		t.Fatalf("register models = %q, want %q", got, want)
	}
	if len(reg.Registrations) != 1 {
		t.Fatalf("registrations = %+v", reg.Registrations)
	}
	if got := modelIDs(reg.Registrations[0].Models); !equalStrings(got, want) {
		t.Fatalf("registration entry models = %q, want %q", got, want)
	}

	var static pluginapi.ModelResponse
	if env := rpc(t, r, "model.static", map[string]any{}); !env.OK {
		t.Fatal(env.Error)
	} else {
		decodeResult(t, env.Result, &static)
	}
	if got := modelIDs(static.Models); !equalStrings(got, want) {
		t.Fatalf("static models = %q, want %q", got, want)
	}

	// Dynamic refresh (per-auth discovery) is filtered the same way.
	var dynamic pluginapi.ModelResponse
	if env := rpc(t, r, "model.for_auth", map[string]any{}); !env.OK {
		t.Fatal(env.Error)
	} else {
		decodeResult(t, env.Result, &dynamic)
	}
	if got := modelIDs(dynamic.Models); !equalStrings(got, want) {
		t.Fatalf("discovered models = %q, want %q", got, want)
	}
}

func TestSecondLayerNamespacePasses(t *testing.T) {
	id := "cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8"
	stub := catalogStub{register: pluginapi.ModelRegistrationResponse{
		Provider: "cloudflare-workers-ai",
		Models:   []pluginapi.ModelInfo{{ID: id}},
	}}
	r := newRuntime(t, namespacedDefinition("cloudflare", stub, stub), nil)
	register(t, r)
	env := rpc(t, r, "model.register", map[string]any{})
	if !env.OK {
		t.Fatal(env.Error)
	}
	var reg pluginapi.ModelRegistrationResponse
	decodeResult(t, env.Result, &reg)
	if got := modelIDs(reg.Models); !equalStrings(got, []string{id}) {
		t.Fatalf("second-layer id dropped: %q", got)
	}
}

func TestCheckModelNamespace(t *testing.T) {
	good := []pluginapi.ModelInfo{{ID: "zai/glm-5.3"}, {ID: "zai/glm-5.3-flash"}}
	if err := CheckModelNamespace("zai", good); err != nil {
		t.Fatalf("good catalog rejected: %v", err)
	}
	bad := []pluginapi.ModelInfo{{ID: "zai/glm-5.3"}, {ID: "glm-5.3"}, {ID: "other/glm-5.3"}, {ID: "zai/zai/glm-5.3"}}
	err := CheckModelNamespace("zai", bad)
	if err == nil {
		t.Fatal("bad catalog accepted")
	}
	for _, id := range []string{"glm-5.3", "other/glm-5.3", "zai/zai/glm-5.3"} {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("error %q does not name %q", err, id)
		}
	}
	if err := CheckModelNamespace("", good); err == nil {
		t.Fatal("empty namespace accepted")
	}
}

type fakeT struct {
	*testing.T
	failures int
}

func (f *fakeT) Helper() {}
func (f *fakeT) Errorf(format string, args ...any) {
	f.failures++
}

func TestAssertModelNamespace(t *testing.T) {
	good := []pluginapi.ModelInfo{{ID: "synthetic/syn:large:text"}}
	ft := &fakeT{}
	AssertModelNamespace(ft, "synthetic", good)
	if ft.failures != 0 {
		t.Fatal("good catalog reported")
	}
	ft = &fakeT{}
	AssertModelNamespace(ft, "synthetic", []pluginapi.ModelInfo{{ID: "bare"}})
	if ft.failures == 0 {
		t.Fatal("bad catalog not reported")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
