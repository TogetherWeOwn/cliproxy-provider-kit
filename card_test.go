package providerkit

import (
	"math"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestUSD(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{25, "$25.00"},
		{7.46, "$7.46"},
		{0, "$0.00"},
		{-0.02, "-$0.02"},
		{9.996, "$10.00"},
		{math.Copysign(0, -1), "$0.00"},
		{-0.001, "-$0.00"},
		// 2.675 is not exact in float64 (it is 2.67499999...), so this
		// documents float formatting, not decimal rounding.
		{2.675, "$2.67"},
		{math.NaN(), "$0.00"},
		{math.Inf(1), "$0.00"},
		{math.Inf(-1), "$0.00"},
	} {
		if got := USD(tc.in); got != tc.want {
			t.Errorf("USD(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCheckCardAcceptsOfficialStyle(t *testing.T) {
	resp := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "OpenRouter", TierName: "paid credits"},
		Summary: []pluginapi.QuotaMetric{
			{Key: "account_balance_usd", Label: "Balance", Value: 7.46, Format: "currency", Currency: "USD"},
			{Key: "lifetime_spent_usd", Label: "Lifetime spent", Value: 237.54, Format: "currency", Currency: "USD"},
		},
		Groups: []pluginapi.QuotaGroup{{DisplayName: "OpenRouter", Buckets: []pluginapi.QuotaBucket{
			{Window: "Weekly key limit", RemainingFraction: 1, ResetTime: "2026-11-09T00:00:00Z"},
			{Window: "Free-model requests today", RemainingFraction: 1, ResetTime: "2026-11-02T00:00:00Z"},
		}}},
	}
	if violations := CheckCard(resp); len(violations) != 0 {
		t.Fatalf("violations: %v", violations)
	}
}

func TestCheckCardRejectsExtras(t *testing.T) {
	resp := pluginapi.QuotaFetchResponse{
		Summary: []pluginapi.QuotaMetric{
			{Key: "a", Label: "Key limit", Value: 1},
			{Key: "b", Label: "Other", Value: 2},
			{Key: "c", Label: "More", Value: 3},
			{Key: "d", Label: "Too many", Value: 4},
		},
		Groups: []pluginapi.QuotaGroup{{DisplayName: "Key credit limit", Buckets: []pluginapi.QuotaBucket{
			{Window: "weekly key limit", RemainingFraction: 1, Description: "$9.996 of $10.00 used"},
		}}},
	}
	violations := CheckCard(resp)
	if len(violations) != 3 {
		t.Fatalf("want 3 violations (description, chip count, repeat), got %v", violations)
	}
}

func TestCheckCardBoundaries(t *testing.T) {
	bar := func(window string) pluginapi.QuotaBucket {
		return pluginapi.QuotaBucket{Window: window, RemainingFraction: 0.5}
	}
	// Exactly MaxCardSummaryChips chips are accepted; one more is not.
	full := pluginapi.QuotaFetchResponse{
		Summary: []pluginapi.QuotaMetric{{Key: "a", Label: "Balance"}, {Key: "b", Label: "Lifetime spent"}, {Key: "c", Label: "Key expires in"}},
		Groups:  []pluginapi.QuotaGroup{{DisplayName: "OpenRouter", Buckets: []pluginapi.QuotaBucket{bar("Weekly key limit")}}},
	}
	if violations := CheckCard(full); len(violations) != 0 {
		t.Fatalf("exactly 3 chips must pass: %v", violations)
	}
	over := full
	over.Summary = append(over.Summary, pluginapi.QuotaMetric{Key: "d", Label: "One too many"})
	if violations := CheckCard(over); len(violations) != 1 {
		t.Fatalf("4 chips must fail: %v", violations)
	}
	// A chip repeating a bar fails however the text overlaps.
	for _, label := range []string{"Weekly key limit", "weekly key", "Weekly key limit remaining"} {
		resp := pluginapi.QuotaFetchResponse{
			Summary: []pluginapi.QuotaMetric{{Key: "x", Label: label}},
			Groups:  []pluginapi.QuotaGroup{{Buckets: []pluginapi.QuotaBucket{bar("Weekly key limit")}}},
		}
		if violations := CheckCard(resp); len(violations) != 1 {
			t.Fatalf("label %q must repeat the bar: %v", label, violations)
		}
	}
	// Whitespace-only descriptions still hide the Window row label.
	blank := pluginapi.QuotaFetchResponse{
		Groups: []pluginapi.QuotaGroup{{Buckets: []pluginapi.QuotaBucket{{Window: "Weekly limit", Description: "  "}}}},
	}
	if violations := CheckCard(blank); len(violations) != 1 {
		t.Fatalf("whitespace description must fail: %v", violations)
	}
}
