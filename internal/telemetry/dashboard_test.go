package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/colinedwardwood/llmtap/internal/genai"
)

// promName mirrors the translation Prometheus's OTLP receiver applies
// to an emitted OTel metric name: dots become underscores and a
// non-annotation unit is appended as a suffix ("s" → "seconds"). UCUM
// annotation units like "{token}" and "{USD}" are dropped entirely —
// which is exactly why the meters use them (see newGenAIMeters).
func promName(otelName, unit string) string {
	name := strings.ReplaceAll(otelName, ".", "_")
	switch unit {
	case "s":
		name += "_seconds"
	case "", "{token}", "{USD}":
		// annotations and unitless: no suffix
	default:
		name += "_" + unit
	}
	return name
}

// TestDashboardQueriesReferenceEmittedMetrics is the regression test
// for review finding R2: the flagship dashboard shipped panels querying
// Prometheus series that llmtap never produced (unit-suffix mangling,
// double _total), so the demo's cost and token panels were empty out of
// the box. Every metric referenced in the dashboard JSON must resolve
// to a series derivable from an instrument this codebase registers.
func TestDashboardQueriesReferenceEmittedMetrics(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{}
	histogram := func(base string) {
		for _, suf := range []string{"_bucket", "_sum", "_count"} {
			allowed[base+suf] = true
		}
	}
	// The GenAI meters, exactly as newGenAIMeters registers them.
	histogram(promName(genai.MetricTokenUsage, "{token}"))
	histogram(promName(genai.MetricOperationDuration, "s"))
	histogram(promName(genai.MetricTimeToFirstToken, "s"))
	histogram(promName(genai.MetricTimeToFirstByte, "s"))
	histogram(promName(genai.MetricCostUSD, "{USD}"))
	allowed[promName(genai.MetricCostUSDTotal, "{USD}")] = true // counter, name already ends _total
	// Proxy self-metrics (internal/proxy.New).
	allowed["llmtap_requests_total"] = true
	allowed["llmtap_requests_in_flight"] = true
	allowed["llmtap_requests_rejected_total"] = true
	allowed["llmtap_auth_rejected_total"] = true
	allowed["llmtap_breaker_state"] = true
	allowed["llmtap_streams_active"] = true
	allowed["llmtap_streams_idle_timeouts_total"] = true
	allowed["llmtap_pricing_misses_total"] = true
	allowed["llmtap_telemetry_export_errors_total"] = true
	allowed[promName("llmtap.tls.cert_not_after", "s")] = true
	allowed["llmtap_tls_cert_reload_failures_total"] = true

	_, thisFile, _, _ := runtime.Caller(0)
	dashPath := filepath.Join(filepath.Dir(thisFile), "..", "..",
		"deploy", "grafana", "dashboards", "llmtap-overview.json")
	raw, err := os.ReadFile(dashPath)
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}

	var dash struct {
		Templating struct {
			List []struct {
				Query struct {
					Query string `json:"query"`
				} `json:"query"`
			} `json:"list"`
		} `json:"templating"`
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("parse dashboard: %v", err)
	}

	// Candidate metric names inside PromQL: identifiers in our two
	// emitted namespaces. Label names (gen_ai_request_model etc.)
	// only appear inside selectors/by-clauses, so require the token
	// to be followed by something other than a comma/paren context —
	// simpler and robust enough: check known label names explicitly.
	metricRe := regexp.MustCompile(`\b(?:gen_ai|llmtap)_[a-zA-Z0-9_]+`)
	labels := map[string]bool{
		"gen_ai_request_model":  true,
		"gen_ai_response_model": true,
		"gen_ai_provider_name":  true,
		"gen_ai_operation_name": true,
	}

	check := func(where, q string) {
		for _, m := range metricRe.FindAllString(q, -1) {
			if labels[m] {
				continue
			}
			if !allowed[m] {
				t.Errorf("%s references %q, which llmtap does not emit (allowed: %v)", where, m, keys(allowed))
			}
		}
	}
	for _, v := range dash.Templating.List {
		check("templating variable", v.Query.Query)
	}
	for _, p := range dash.Panels {
		for _, tgt := range p.Targets {
			check("panel "+p.Title, tgt.Expr)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
