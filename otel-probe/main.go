// Command otel-probe is a minimal OTLP/HTTP-JSON receiver for spike S2.
//
// Question S2 answers: with all content-logging flags OFF, does Claude Code's
// telemetry constitute a defensible audit trail for an enterprise customer?
// If not, the "proxy the CLI instead of owning the client" decision in
// CC_REMOTE_ANALYSIS.md breaks and Phase 4 moves up.
//
// It also verifies the two things that matter operationally:
//   - per-tenant attribution via OTEL_RESOURCE_ATTRIBUTES actually lands on
//     every metric and event
//   - prompt/response content really is redacted by default
//
// Usage:
//
//	otel-probe -addr :4318 -out ./capture
//
// Then run Claude Code with:
//
//	CLAUDE_CODE_ENABLE_TELEMETRY=1 \
//	OTEL_METRICS_EXPORTER=otlp OTEL_LOGS_EXPORTER=otlp \
//	OTEL_EXPORTER_OTLP_PROTOCOL=http/json \
//	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
//	OTEL_METRIC_EXPORT_INTERVAL=5000 \
//	OTEL_RESOURCE_ATTRIBUTES=tenant.id=acme,workspace.id=ws_001 \
//	claude
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var (
	addr    = flag.String("addr", ":4318", "listen address")
	outDir  = flag.String("out", "./capture", "directory for raw payload dumps")
	verbose = flag.Bool("v", false, "print every event as it arrives")
)

type stats struct {
	mu sync.Mutex

	// event name -> attribute key -> set of observed values (capped)
	events map[string]map[string]map[string]int
	// metric name -> attribute key -> set of observed values
	metrics map[string]map[string]map[string]int

	resourceAttrs map[string]map[string]int
	nPayloads     int
}

func newStats() *stats {
	return &stats{
		events:        map[string]map[string]map[string]int{},
		metrics:       map[string]map[string]map[string]int{},
		resourceAttrs: map[string]map[string]int{},
	}
}

// anyValue flattens the OTLP AnyValue union into a display string.
func anyValue(v map[string]any) string {
	for _, k := range []string{"stringValue", "boolValue", "intValue", "doubleValue"} {
		if x, ok := v[k]; ok {
			return fmt.Sprint(x)
		}
	}
	if x, ok := v["arrayValue"]; ok {
		return fmt.Sprintf("%v", x)
	}
	return ""
}

func kvList(raw any) map[string]string {
	out := map[string]string{}
	list, ok := raw.([]any)
	if !ok {
		return out
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, _ := m["key"].(string)
		val, _ := m["value"].(map[string]any)
		out[key] = anyValue(val)
	}
	return out
}

func (s *stats) recordAttrs(dst map[string]map[string]int, attrs map[string]string) {
	for k, v := range attrs {
		if dst[k] == nil {
			dst[k] = map[string]int{}
		}
		// Cap distinct values so high-cardinality keys (session.id, prompt.id)
		// don't blow up the report.
		if len(dst[k]) < 6 {
			dst[k][v]++
		} else {
			dst[k]["<more>"]++
		}
	}
}

func (s *stats) ingestLogs(body []byte) {
	var payload struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes any `json:"attributes"`
			} `json:"resource"`
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes any    `json:"attributes"`
					Body       any    `json:"body"`
					TimeUnix   string `json:"timeUnixNano"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("logs unmarshal: %v", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rl := range payload.ResourceLogs {
		s.recordAttrs(s.resourceAttrs, kvList(rl.Resource.Attributes))
		for _, sl := range rl.ScopeLogs {
			for _, rec := range sl.LogRecords {
				attrs := kvList(rec.Attributes)
				name := attrs["event.name"]
				if name == "" {
					if b, ok := rec.Body.(map[string]any); ok {
						name = anyValue(b)
					}
				}
				if name == "" {
					name = "<unnamed>"
				}
				if s.events[name] == nil {
					s.events[name] = map[string]map[string]int{}
				}
				s.recordAttrs(s.events[name], attrs)
				if *verbose {
					j, _ := json.Marshal(attrs)
					fmt.Printf("EVENT %s %s\n", name, j)
				}
			}
		}
	}
}

func (s *stats) ingestMetrics(body []byte) {
	var payload struct {
		ResourceMetrics []struct {
			Resource struct {
				Attributes any `json:"attributes"`
			} `json:"resource"`
			ScopeMetrics []struct {
				Metrics []struct {
					Name string `json:"name"`
					Unit string `json:"unit"`
					Sum  struct {
						DataPoints []struct {
							Attributes any `json:"attributes"`
						} `json:"dataPoints"`
					} `json:"sum"`
					Gauge struct {
						DataPoints []struct {
							Attributes any `json:"attributes"`
						} `json:"dataPoints"`
					} `json:"gauge"`
					Histogram struct {
						DataPoints []struct {
							Attributes any `json:"attributes"`
						} `json:"dataPoints"`
					} `json:"histogram"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("metrics unmarshal: %v", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rm := range payload.ResourceMetrics {
		s.recordAttrs(s.resourceAttrs, kvList(rm.Resource.Attributes))
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if s.metrics[m.Name] == nil {
					s.metrics[m.Name] = map[string]map[string]int{}
				}
				for _, dps := range [][]struct {
					Attributes any `json:"attributes"`
				}{m.Sum.DataPoints, m.Gauge.DataPoints, m.Histogram.DataPoints} {
					for _, dp := range dps {
						s.recordAttrs(s.metrics[m.Name], kvList(dp.Attributes))
					}
				}
				if *verbose {
					fmt.Printf("METRIC %s (unit=%q)\n", m.Name, m.Unit)
				}
			}
		}
	}
}

func (s *stats) report(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Fprintf(w, "\n===== S2 CAPTURE REPORT =====\n")
	fmt.Fprintf(w, "payloads received: %d\n", s.nPayloads)

	fmt.Fprintf(w, "\n--- RESOURCE ATTRIBUTES (per-tenant attribution) ---\n")
	printAttrTable(w, s.resourceAttrs)

	fmt.Fprintf(w, "\n--- METRICS (%d distinct) ---\n", len(s.metrics))
	for _, name := range sortedKeys(s.metrics) {
		fmt.Fprintf(w, "\n  %s\n", name)
		printAttrTableIndent(w, s.metrics[name], "    ")
	}

	fmt.Fprintf(w, "\n--- EVENTS (%d distinct) ---\n", len(s.events))
	for _, name := range sortedKeys(s.events) {
		fmt.Fprintf(w, "\n  %s\n", name)
		printAttrTableIndent(w, s.events[name], "    ")
	}

	// The privacy assertion. If any of these carry real text rather than a
	// redaction marker, the default posture is not what the docs claim and the
	// security story in CC_REMOTE_ANALYSIS.md 5.4 needs revisiting.
	fmt.Fprintf(w, "\n--- PRIVACY CHECK (content flags OFF) ---\n")
	// Finding F1: event names arrive WITHOUT the documented "claude_code."
	// prefix (metrics keep it, events do not). Probing the prefixed names
	// matched nothing and printed a misleading all-clear.
	for _, probe := range []struct{ event, attr string }{
		{"user_prompt", "prompt"},
		{"assistant_response", "response"},
		{"tool_result", "tool_parameters"},
		{"tool_result", "tool_input"},
		{"tool_decision", "tool_parameters"},
	} {
		vals, ok := s.events[probe.event][probe.attr]
		switch {
		case !ok:
			fmt.Fprintf(w, "  OK      %s.%s absent\n", probe.event, probe.attr)
		default:
			for v := range vals {
				marker := "LEAK?"
				if v == "" || strings.Contains(v, "REDACTED") {
					marker = "OK   "
				}
				fmt.Fprintf(w, "  %s %s.%s = %q\n", marker, probe.event, probe.attr, truncate(v, 80))
			}
		}
	}
	fmt.Fprintln(w)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func printAttrTable(w io.Writer, m map[string]map[string]int) { printAttrTableIndent(w, m, "  ") }
func printAttrTableIndent(w io.Writer, m map[string]map[string]int, indent string) {
	if len(m) == 0 {
		fmt.Fprintf(w, "%s(none)\n", indent)
		return
	}
	for _, k := range sortedKeys(m) {
		var vals []string
		for v := range m[k] {
			vals = append(vals, truncate(v, 40))
		}
		sort.Strings(vals)
		if len(vals) > 4 {
			vals = append(vals[:4], fmt.Sprintf("…+%d", len(vals)-4))
		}
		fmt.Fprintf(w, "%s%-34s %s\n", indent, k, strings.Join(vals, " | "))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func main() {
	flag.Parse()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	s := newStats()
	var seq int
	var seqMu sync.Mutex

	handle := func(kind string, ingest func([]byte)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			seqMu.Lock()
			seq++
			n := seq
			seqMu.Unlock()

			// Dump raw so the exact wire payload is inspectable afterwards.
			path := filepath.Join(*outDir, fmt.Sprintf("%04d-%s.json", n, kind))
			_ = os.WriteFile(path, body, 0o644)

			s.mu.Lock()
			s.nPayloads++
			s.mu.Unlock()
			ingest(body)

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}
	}

	http.HandleFunc("/v1/logs", handle("logs", s.ingestLogs))
	http.HandleFunc("/v1/metrics", handle("metrics", s.ingestMetrics))
	http.HandleFunc("/v1/traces", handle("traces", func([]byte) {}))
	// GET /report prints the analysis without stopping the collector.
	http.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		s.report(w)
		s.report(os.Stdout)
	})

	log.Printf("otel-probe listening on %s, dumping to %s", *addr, *outDir)
	log.Printf("when done: curl -s localhost%s/report", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
