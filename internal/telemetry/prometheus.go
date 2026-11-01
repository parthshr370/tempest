package telemetry

import (
	"bufio"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// prometheusContentType is the text exposition format version 0.0.4.
const prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

func formatInt(value int64) string { return strconv.FormatInt(value, 10) }

func formatUint(value uint64) string { return strconv.FormatUint(value, 10) }

func formatFloat(value float64) string {
	switch {
	case math.IsInf(value, 1):
		return "+Inf"
	case math.IsInf(value, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// prometheusName maps an OpenTelemetry metric name to Prometheus: dots and
// other invalid characters become underscores, the unit becomes a suffix as
// the OpenTelemetry-Prometheus compatibility rules describe (annotations in
// braces are dropped), and counters end in _total.
func prometheusName(inst instrumentSnapshot) string {
	name := sanitizePrometheus(inst.name)
	switch inst.unit {
	case "s":
		name += "_seconds"
	case "USD":
		name += "_usd"
	}
	if inst.kind == kindCounter {
		name += "_total"
	}
	return name
}

func sanitizePrometheus(name string) string {
	var sanitized strings.Builder
	for i, character := range name {
		valid := character == '_' || character == ':' || (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (i > 0 && character >= '0' && character <= '9')
		if valid {
			sanitized.WriteRune(character)
		} else {
			sanitized.WriteByte('_')
		}
	}
	return sanitized.String()
}

func escapeLabelValue(value string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(value)
}

func escapeHelp(value string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(value)
}

// writeLabels renders {k="v",...} with an optional trailing le label.
func writeLabels(out *bufio.Writer, attrs []attribute, le string) {
	if len(attrs) == 0 && le == "" {
		return
	}
	out.WriteByte('{')
	for i, attr := range attrs {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(sanitizePrometheus(attr.key))
		out.WriteString(`="`)
		out.WriteString(escapeLabelValue(attr.text))
		out.WriteByte('"')
	}
	if le != "" {
		if len(attrs) > 0 {
			out.WriteByte(',')
		}
		out.WriteString(`le="`)
		out.WriteString(le)
		out.WriteByte('"')
	}
	out.WriteByte('}')
}

// writePrometheus renders a snapshot in the Prometheus text format.
func writePrometheus(w io.Writer, collected snapshot) error {
	out := bufio.NewWriter(w)
	for _, inst := range collected.instruments {
		if len(inst.series) == 0 {
			continue
		}
		name := prometheusName(inst)
		out.WriteString("# HELP " + name + " " + escapeHelp(inst.description) + "\n")
		switch inst.kind {
		case kindCounter:
			out.WriteString("# TYPE " + name + " counter\n")
		case kindGauge:
			out.WriteString("# TYPE " + name + " gauge\n")
		case kindHistogram:
			out.WriteString("# TYPE " + name + " histogram\n")
		}
		for _, entry := range inst.series {
			if inst.kind != kindHistogram {
				out.WriteString(name)
				writeLabels(out, entry.attributes, "")
				out.WriteString(" " + formatInt(entry.value) + "\n")
				continue
			}
			var cumulative uint64
			for b, count := range entry.buckets {
				cumulative += count
				le := "+Inf"
				if b < len(inst.bounds) {
					le = formatFloat(inst.bounds[b])
				}
				out.WriteString(name + "_bucket")
				writeLabels(out, entry.attributes, le)
				out.WriteString(" " + formatUint(cumulative) + "\n")
			}
			out.WriteString(name + "_sum")
			writeLabels(out, entry.attributes, "")
			out.WriteString(" " + formatFloat(entry.sum) + "\n")
			out.WriteString(name + "_count")
			writeLabels(out, entry.attributes, "")
			out.WriteString(" " + formatUint(entry.count) + "\n")
		}
	}
	return out.Flush()
}

// MetricsHandler serves the metrics as Prometheus text. The caller mounts it
// behind its own authentication.
func (t *Telemetry) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", prometheusContentType)
		if r.Method == http.MethodHead {
			return
		}
		if err := writePrometheus(w, t.metrics.collect(time.Now())); err != nil {
			// The scraper went away mid-response; nothing can be sent back.
			slog.Debug("metrics scrape write failed", "error", err)
		}
	})
}
