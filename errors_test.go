package providerkit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

type transportError struct {
	status    int
	after     *time.Duration
	retryable bool
}

func (e transportError) Error() string              { return "POST https://example.com/v1?key=example-key failed" }
func (e transportError) StatusCode() int            { return e.status }
func (e transportError) RetryAfter() *time.Duration { return e.after }
func (e transportError) Retryable() bool            { return e.retryable }

type credentialScopedError struct {
	transportError
	scoped bool
}

func (e credentialScopedError) CredentialScoped() bool { return e.scoped }

type hostScopedError struct {
	transportError
	scoped bool
}

func (e hostScopedError) IsCredentialScoped() bool { return e.scoped }

func TestMapErrorNormalizesPublicErrors(t *testing.T) {
	if MapError(nil) != nil {
		t.Fatal("nil error mapped")
	}
	for _, tc := range []struct {
		in   pluginabi.Error
		want pluginabi.Error
	}{
		{pluginabi.Error{}, pluginabi.Error{Code: "plugin_error", Message: "plugin operation failed", HTTPStatus: 500}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 200}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 500}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 399}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 500}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 600}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 500}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 400}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 400}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 599}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 599}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 429, RetryAfterSeconds: -5, Retryable: true}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 429, Retryable: true}},
	} {
		in := tc.in
		if got := MapError(&in); *got != tc.want {
			t.Fatalf("MapError(%+v) = %+v, want %+v", tc.in, *got, tc.want)
		}
		// Normalization must not rewrite a provider-owned (possibly shared) error value.
		if in != tc.in {
			t.Fatalf("input mutated: %+v", in)
		}
	}
}

func TestMapErrorCredentialScoped(t *testing.T) {
	after := 90 * time.Second
	scoped := credentialScopedError{transportError: transportError{status: 429, after: &after}, scoped: true}
	e := MapError(fmt.Errorf("wrapped: %w", scoped))
	if e.HTTPStatus != 429 || !e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("credential-scoped 429 %+v", e)
	}
	// An explicit false stays unscoped.
	e = MapError(credentialScopedError{transportError: transportError{status: 429, after: &after}, scoped: false})
	if e.HTTPStatus != 429 || e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("unscoped 429 %+v", e)
	}
	// The host's native naming works too.
	e = MapError(hostScopedError{transportError: transportError{status: 429, after: &after}, scoped: true})
	if e.HTTPStatus != 429 || !e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("host-scoped 429 %+v", e)
	}
	// An unscoped 429 without a scope hook is unchanged.
	e = MapError(transportError{status: 429, after: &after})
	if e.HTTPStatus != 429 || e.CredentialScoped || e.RetryAfterSeconds != 90 {
		t.Fatalf("plain 429 %+v", e)
	}
	// A scoped 429 without retry-after keeps the zero hint.
	e = MapError(credentialScopedError{transportError: transportError{status: 429}, scoped: true})
	if e.HTTPStatus != 429 || !e.CredentialScoped || e.RetryAfterSeconds != 0 {
		t.Fatalf("scoped 429 without retry-after %+v", e)
	}
	// An explicit pluginabi.Error passes its scope through normalization.
	in := pluginabi.Error{Code: "rate_limit_exceeded", Message: "quota exhausted", HTTPStatus: 429, RetryAfterSeconds: 120, CredentialScoped: true}
	if got := MapError(&in); !got.CredentialScoped || got.RetryAfterSeconds != 120 || got.HTTPStatus != 429 {
		t.Fatalf("pluginabi.Error passthrough %+v", got)
	}
	wire, err := json.Marshal(MapError(scoped))
	if err != nil || !strings.Contains(string(wire), `"credential_scoped":true`) {
		t.Fatalf("wire form %s, err %v", wire, err)
	}
}

func TestMapErrorContextAndOpaqueTransportErrors(t *testing.T) {
	e := MapError(fmt.Errorf("dial https://example.com/?key=example-key: %w", context.Canceled))
	if e.Code != "canceled" || e.HTTPStatus != 499 || e.Retryable || strings.Contains(e.Message, "example-key") {
		t.Fatalf("canceled %+v", e)
	}
	e = MapError(fmt.Errorf("read https://example.com/?key=example-key: %w", context.DeadlineExceeded))
	if e.Code != "timeout" || e.HTTPStatus != 504 || !e.Retryable || strings.Contains(e.Message, "example-key") {
		t.Fatalf("deadline %+v", e)
	}
	after := 1500 * time.Millisecond
	e = MapError(fmt.Errorf("wrapped: %w", transportError{status: 503, after: &after, retryable: true}))
	if e.Code != "plugin_error" || e.Message != "plugin operation failed" || e.HTTPStatus != 503 || e.RetryAfterSeconds != 2 || !e.Retryable {
		t.Fatalf("classified transport error %+v", e)
	}
	zero := time.Duration(0)
	for _, in := range []transportError{{status: 200}, {status: 302, after: &zero}, {status: 700}} {
		if e = MapError(in); e.HTTPStatus != 500 || e.RetryAfterSeconds != 0 || e.Retryable || strings.Contains(e.Message, "example-key") {
			t.Fatalf("MapError(%+v) = %+v", in, e)
		}
	}
}

func TestRetryAfterSecondsEdges(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		s    string
		want int
	}{
		{" 30 ", 30},
		{"0", 0},
		{"", 0},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{now.Format(http.TimeFormat), 0},
		{now.Add(100 * 365 * 24 * time.Hour).Format(http.TimeFormat), math.MaxInt32},
	} {
		if got := RetryAfterSeconds(tc.s, now); got != tc.want {
			t.Fatalf("RetryAfterSeconds(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
	// HTTP dates have one-second resolution; a fraction of a second left still means wait.
	if got := RetryAfterSeconds(now.Add(5*time.Second).Format(http.TimeFormat), now.Add(200*time.Millisecond)); got != 5 {
		t.Fatalf("partial second rounded down: %d", got)
	}
}

func TestResetRetryAfterSeconds(t *testing.T) {
	now := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)
	// Meta's live shape: the reset time is embedded in the message text.
	meta := `{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at 2026-11-11T02:59:37Z."}`
	if got := ResetRetryAfterSeconds([]byte(meta), now); got != 3577 {
		t.Fatalf("meta body: %d, want 3577", got)
	}
	// Unix-epoch resets_at, nested and top-level.
	if got := ResetRetryAfterSeconds([]byte(`{"error":{"resets_at":1767226200}}`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); got != 600 {
		t.Fatalf("nested resets_at: %d, want 600", got)
	}
	if got := ResetRetryAfterSeconds([]byte(`{"resets_at":"2026-01-01T00:10:00Z"}`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); got != 600 {
		t.Fatalf("top-level resets_at string: %d, want 600", got)
	}
	// Raw-text (non-JSON) fallback.
	if got := ResetRetryAfterSeconds([]byte(`quota resets at 2026-11-11T02:05:00Z please retry`), now); got != 300 {
		t.Fatalf("raw text: %d, want 300", got)
	}
	// A fraction of a second left still means wait.
	if got := ResetRetryAfterSeconds([]byte(`{"resets_at":"2026-11-11T02:00:05.2Z"}`), now.Add(100*time.Millisecond)); got != 6 {
		t.Fatalf("partial second rounded down: %d", got)
	}
	for _, tc := range []struct {
		name string
		body string
		at   time.Time
	}{
		{"empty", "", now},
		{"no timestamp", `{"code":"rate_limit_exceeded","message":"slow down"}`, now},
		{"past reset", `{"error":{"resets_at":1767225600}}`, time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)},
		{"past text", `{"message":"resets at 2026-01-01T00:00:00Z"}`, now},
	} {
		if got := ResetRetryAfterSeconds([]byte(tc.body), tc.at); got != 0 {
			t.Fatalf("%s: %d, want 0", tc.name, got)
		}
	}
	// Far-future resets clamp like RetryAfterSeconds.
	far := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	if got := ResetRetryAfterSeconds([]byte(fmt.Sprintf(`{"error":{"resets_at":%d}}`, far)), now); got != math.MaxInt32 {
		t.Fatalf("far future: %d, want MaxInt32", got)
	}
}
