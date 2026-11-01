// Card helpers keep every provider quota card close to the official
// provider card style (one short plan chip, one row per real limit window,
// no group-specific chrome, dollar balances as text or chips, never bars).
//
// The management panel renders plugin cards generically: every summary metric
// becomes a chip, each group gets a header, and a bucket's Description becomes
// its row label (the SDK QuotaBucket has no display name). So card code must:
//   - leave Description empty and put the short row label in Window;
//   - emit at most MaxCardSummaryChips summary metrics, none repeating a bar;
//   - format dollar amounts in text with USD (exactly 2 decimals).
package providerkit

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Canonical short row labels for quota windows, matching the official
// provider cards. Keep Window values short: the panel sorts plugin buckets by
// window for the well-known values and falls back to label order, so staging
// screenshots must confirm multi-window order after relabelling.
const (
	CardWindowFiveHour = "5-hour limit"
	CardWindowWeekly   = "Weekly limit"
	CardWindowMonthly  = "Monthly limit"
	CardWindowDaily    = "Daily limit"
)

// MaxCardSummaryChips bounds the plan-row chip count (plan chip excluded).
// The panel renders every summary metric as a chip with no limit, so an
// unbounded summary becomes a wall of chips next to the official cards.
const MaxCardSummaryChips = 3

// USD formats dollars with exactly 2 decimals, sign first:
// 25 -> $25.00, -0.02 -> -$0.02. Amounts in bucket text must use this, never
// a higher-precision debug format; chip currency metrics are formatted
// panel-side from the numeric value.
func USD(v float64) string {
	if v == 0 {
		return "$0.00"
	}
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return sign + fmt.Sprintf("$%.2f", v)
}

// CheckCard reports official-card-style violations in a quota response:
//   - any bucket Description (the panel renders it as the row label);
//   - more than MaxCardSummaryChips summary metrics;
//   - a summary metric repeating a bar (same or overlapping label text).
//
// An empty return means the card follows the shared style. Plugin quota tests
// assert on this alongside their exact-shape golden tests.
func CheckCard(resp pluginapi.QuotaFetchResponse) []string {
	var violations []string
	for gi, g := range resp.Groups {
		for _, b := range g.Buckets {
			if strings.TrimSpace(b.Description) != "" {
				violations = append(violations, fmt.Sprintf("groups[%d] bucket (%q) sets Description; leave it empty so Window stays the row label", gi, b.Window))
			}
		}
	}
	if len(resp.Summary) > MaxCardSummaryChips {
		violations = append(violations, fmt.Sprintf("summary has %d metrics, max %d", len(resp.Summary), MaxCardSummaryChips))
	}
	windows := make([]string, 0, len(resp.Groups))
	for _, g := range resp.Groups {
		for _, b := range g.Buckets {
			if w := strings.ToLower(strings.TrimSpace(b.Window)); w != "" {
				windows = append(windows, w)
			}
		}
	}
	for _, m := range resp.Summary {
		label := strings.ToLower(strings.TrimSpace(m.Label))
		for _, w := range windows {
			if label == "" || w == "" {
				continue
			}
			if label == w || strings.Contains(label, w) || strings.Contains(w, label) {
				violations = append(violations, fmt.Sprintf("summary %q repeats bar %q", m.Key, w))
			}
		}
	}
	return violations
}
