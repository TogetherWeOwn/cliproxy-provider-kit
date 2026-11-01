package providerkit

import (
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Owner rule (2026-11-01): any provider that serves models made by another
// vendor must namespace its model ids, so they never clash when the official
// provider is added later. A plugin declares its fixed namespace once in its
// Definition; the kit then refuses to register any model whose id does not
// carry that namespace exactly once. The check covers static, discovered and
// dynamic lists alike: an offending model is dropped with a logged error and
// is never served bare.
//
// A bare id ("gpt-oss-20b"), a foreign namespace ("other/gpt-oss-20b") and a
// doubled namespace ("nvidia/nvidia/gpt-oss-20b", usually a plugin prefixing
// an already-prefixed upstream id) are all dropped. A second layer past the
// namespace ("cloudflare/@cf/meta/llama-3.1-8b-instruct-fp8") passes: only
// the leading "<namespace>/" is the kit's concern, and the remainder is
// passed to the upstream untouched.
var namespacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateModelNamespace reports whether ns is usable as a fixed model
// namespace: a bare segment such as "synthetic", without the trailing slash.
func ValidateModelNamespace(ns string) error {
	if strings.TrimSpace(ns) != ns || ns == "" {
		return fmt.Errorf("model namespace must be a non-empty segment without surrounding whitespace")
	}
	if !namespacePattern.MatchString(ns) {
		return fmt.Errorf("model namespace %q must match %s", ns, namespacePattern)
	}
	return nil
}

// namespaced reports whether id carries ns exactly once as its leading
// segment: "<ns>/<rest>" where rest is non-empty and does not itself start
// with "<ns>/".
func namespaced(ns, id string) bool {
	rest, ok := strings.CutPrefix(id, ns+"/")
	if !ok || rest == "" || rest == ns {
		return false
	}
	return !strings.HasPrefix(rest, ns+"/")
}

// filterModels drops every model whose id is not namespaced under ns, logging
// each drop. It returns the surviving models, never a bare id, without
// mutating the provider's slice.
func filterModels(ns string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if len(models) == 0 {
		return models
	}
	kept := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if !namespaced(ns, m.ID) {
			log.Printf("providerkit: dropping model %.200q: id must start with %q exactly once", m.ID, ns+"/")
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// filterRegistrations filters every entry's models without mutating the
// provider's backing array: the Registrations slice itself is copied first,
// so a registrar reusing a cached response never sees its catalog truncated
// and concurrent registrations cannot race on shared entries.
func filterRegistrations(ns string, regs []pluginapi.ProviderModels) []pluginapi.ProviderModels {
	if len(regs) == 0 {
		return regs
	}
	out := make([]pluginapi.ProviderModels, len(regs))
	copy(out, regs)
	for i := range out {
		out[i].Models = filterModels(ns, out[i].Models)
	}
	return out
}

// CheckModelNamespace is the reusable conformance check for plugin test
// suites: it reports an error listing every model id that would not be
// registered under ns. Call it with every id the plugin can emit (static
// fallback, discovery fixtures and dynamic refresh paths).
func CheckModelNamespace(ns string, models []pluginapi.ModelInfo) error {
	if err := ValidateModelNamespace(ns); err != nil {
		return err
	}
	var bad []string
	for _, m := range models {
		if !namespaced(ns, m.ID) {
			bad = append(bad, m.ID)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("models missing the %q namespace: %s", ns+"/", strings.Join(bad, ", "))
	}
	return nil
}

// TestReporter is the subset of testing.T used by AssertModelNamespace, so
// plugin suites in any harness can use the conformance check.
type TestReporter interface {
	Helper()
	Errorf(string, ...any)
}

// AssertModelNamespace fails the test if any model id would not be registered
// under ns.
func AssertModelNamespace(t TestReporter, ns string, models []pluginapi.ModelInfo) {
	t.Helper()
	if err := CheckModelNamespace(ns, models); err != nil {
		t.Errorf("%v", err)
	}
}
