package providerkit

import (
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
