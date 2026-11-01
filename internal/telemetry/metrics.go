package telemetry

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// Metrics are aggregated in memory with cumulative temporality and read two
// ways: pushed over OTLP every metric export interval, and scraped as
// Prometheus text from `serve -listen` at /metrics. Recording is a short
// critical section on the turn loop's goroutine; collection, encoding and
// network I/O run on the exporter or the scraping request.

type instrumentKind int

const (
	kindCounter instrumentKind = iota
	kindHistogram
	kindGauge
)

// aggregationCumulative is the OTLP AggregationTemporality enum value.
const aggregationCumulative = 2

var (
	// tokenBuckets and durationBuckets are the explicit bucket boundaries
	// the OpenTelemetry GenAI conventions recommend for
	// gen_ai.client.token.usage and gen_ai.client.operation.duration.
	tokenBuckets    = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
	durationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	costBuckets     = []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10}
)

// instrument is one metric. Counters and histograms keep one series per
// attribute set; an observed instrument reads its points at collection.
type instrument struct {
	name        string
	unit        string
	description string
	kind        instrumentKind
	bounds      []float64
	series      map[string]*series
	order       []string
	observe     func() []observation
}

// observation is one point read from live state at collection time.
type observation struct {
	attributes []attribute
	value      int64
}

type series struct {
	attributes []attribute
	count      uint64
	sum        float64
	min        float64
	max        float64
	buckets    []uint64
	value      int64
}

// metricSet is the process's instruments. Metric attributes are always
// strings, which keeps series keys and Prometheus labels simple.
type metricSet struct {
	mu          sync.Mutex
	start       time.Time
	instruments []*instrument

	tokenUsage       *instrument
	duration         *instrument
	timeToFirstChunk *instrument
	cost             *instrument
	runs             *instrument
	toolCalls        *instrument
	failedAttempts   *instrument
	permissions      *instrument
	hookRuns         *instrument
}

func newMetricSet(start time.Time) *metricSet {
	set := &metricSet{start: start}
	set.tokenUsage = set.histogram("gen_ai.client.token.usage", "{token}", "Input and output tokens per model request.", tokenBuckets)
	set.duration = set.histogram("gen_ai.client.operation.duration", "s", "Model request duration, retries included.", durationBuckets)
	set.timeToFirstChunk = set.histogram("gen_ai.client.operation.time_to_first_chunk", "s", "Time from sending a request attempt to its first response chunk.", durationBuckets)
	set.cost = set.histogram("gen_ai.client.operation.cost", "USD", "Model request cost in US dollars; requests with an unknown price are not recorded.", costBuckets)
	set.runs = set.counter("tempest.agent.runs", "{run}", "Agent runs by how they ended.")
	set.toolCalls = set.counter("tempest.tool.calls", "{call}", "Tool calls by tool and outcome.")
	set.failedAttempts = set.counter("tempest.model.failed_attempts", "{attempt}", "Model request attempts that failed and were sent again, by error type and outcome.")
	set.permissions = set.counter("tempest.permission.decisions", "{decision}", "Tool permission decisions by outcome and source.")
	set.hookRuns = set.counter("tempest.hook.runs", "{run}", "Hook command runs by event and outcome.")
	return set
}

func (s *metricSet) add(inst *instrument) *instrument {
	inst.series = map[string]*series{}
	s.instruments = append(s.instruments, inst)
	return inst
}

func (s *metricSet) counter(name, unit, description string) *instrument {
	return s.add(&instrument{name: name, unit: unit, description: description, kind: kindCounter})
}

func (s *metricSet) histogram(name, unit, description string, bounds []float64) *instrument {
	return s.add(&instrument{name: name, unit: unit, description: description, kind: kindHistogram, bounds: bounds})
}

// observed registers an instrument whose points come from live state.
func (s *metricSet) observed(name, unit, description string, kind instrumentKind, observe func() []observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.add(&instrument{name: name, unit: unit, description: description, kind: kind, observe: observe})
}

func seriesKey(attrs []attribute) string {
	var key strings.Builder
	for _, attr := range attrs {
		key.WriteString(attr.key)
		key.WriteByte(0)
		key.WriteString(attr.text)
		key.WriteByte(1)
	}
	return key.String()
}

func (s *metricSet) seriesFor(inst *instrument, attrs []attribute) *series {
	key := seriesKey(attrs)
	entry, ok := inst.series[key]
	if !ok {
		entry = &series{attributes: attrs}
		if inst.kind == kindHistogram {
			entry.buckets = make([]uint64, len(inst.bounds)+1)
		}
		inst.series[key] = entry
		inst.order = append(inst.order, key)
	}
	return entry
}

func (s *metricSet) increment(inst *instrument, attrs ...attribute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seriesFor(inst, attrs).value++
}

func (s *metricSet) observe(inst *instrument, value float64, attrs ...attribute) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.seriesFor(inst, attrs)
	if entry.count == 0 || value < entry.min {
		entry.min = value
	}
	if entry.count == 0 || value > entry.max {
		entry.max = value
	}
	entry.count++
	entry.sum += value
	bucket, _ := slices.BinarySearch(inst.bounds, value)
	entry.buckets[bucket]++
}

// snapshot is a consistent copy of every instrument for one export or scrape.
type snapshot struct {
	at          time.Time
	start       time.Time
	instruments []instrumentSnapshot
}

type instrumentSnapshot struct {
	name        string
	unit        string
	description string
	kind        instrumentKind
	bounds      []float64
	series      []series
}

func (s *metricSet) collect(now time.Time) snapshot {
	s.mu.Lock()
	instruments := slices.Clone(s.instruments)
	result := snapshot{at: now, start: s.start, instruments: make([]instrumentSnapshot, 0, len(instruments))}
	var observedIndexes []int
	for _, inst := range instruments {
		copied := instrumentSnapshot{name: inst.name, unit: inst.unit, description: inst.description, kind: inst.kind, bounds: inst.bounds}
		for _, key := range inst.order {
			entry := *inst.series[key]
			entry.buckets = slices.Clone(entry.buckets)
			copied.series = append(copied.series, entry)
		}
		if inst.observe != nil {
			observedIndexes = append(observedIndexes, len(result.instruments))
		}
		result.instruments = append(result.instruments, copied)
	}
	s.mu.Unlock()
	// Observers read other components' state; they run outside the lock so a
	// slow observer never delays recording.
	for _, index := range observedIndexes {
		for _, point := range instruments[index].observe() {
			result.instruments[index].series = append(result.instruments[index].series, series{attributes: point.attributes, value: point.value})
		}
	}
	return result
}

type wireNumberPoint struct {
	Attributes        []wireKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	AsInt             string         `json:"asInt"`
}

type wireHistogramPoint struct {
	Attributes        []wireKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	Count             string         `json:"count"`
	Sum               float64        `json:"sum"`
	BucketCounts      []string       `json:"bucketCounts"`
	ExplicitBounds    []float64      `json:"explicitBounds"`
	Min               *float64       `json:"min,omitempty"`
	Max               *float64       `json:"max,omitempty"`
}

type wireSum struct {
	DataPoints             []wireNumberPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

type wireGauge struct {
	DataPoints []wireNumberPoint `json:"dataPoints"`
}

type wireHistogram struct {
	DataPoints             []wireHistogramPoint `json:"dataPoints"`
	AggregationTemporality int                  `json:"aggregationTemporality"`
}

type wireMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Sum         *wireSum       `json:"sum,omitempty"`
	Gauge       *wireGauge     `json:"gauge,omitempty"`
	Histogram   *wireHistogram `json:"histogram,omitempty"`
}

type wireScopeMetrics struct {
	Scope   wireScope    `json:"scope"`
	Metrics []wireMetric `json:"metrics"`
}

type wireResourceMetrics struct {
	Resource     wireResource       `json:"resource"`
	ScopeMetrics []wireScopeMetrics `json:"scopeMetrics"`
}

type wireMetricsRequest struct {
	ResourceMetrics []wireResourceMetrics `json:"resourceMetrics"`
}

// encodeMetrics renders one ExportMetricsServiceRequest. Instruments without
// points are left out: an empty metric carries nothing.
func encodeMetrics(res resource, collected snapshot) ([]byte, error) {
	start, now := unixNano(collected.start), unixNano(collected.at)
	metrics := make([]wireMetric, 0, len(collected.instruments))
	for _, inst := range collected.instruments {
		if len(inst.series) == 0 {
			continue
		}
		metric := wireMetric{Name: inst.name, Description: inst.description, Unit: inst.unit}
		switch inst.kind {
		case kindCounter:
			points := make([]wireNumberPoint, len(inst.series))
			for i, entry := range inst.series {
				points[i] = wireNumberPoint{Attributes: wireAttributes(entry.attributes), StartTimeUnixNano: start, TimeUnixNano: now, AsInt: formatInt(entry.value)}
			}
			metric.Sum = &wireSum{DataPoints: points, AggregationTemporality: aggregationCumulative, IsMonotonic: true}
		case kindGauge:
			points := make([]wireNumberPoint, len(inst.series))
			for i, entry := range inst.series {
				points[i] = wireNumberPoint{Attributes: wireAttributes(entry.attributes), TimeUnixNano: now, AsInt: formatInt(entry.value)}
			}
			metric.Gauge = &wireGauge{DataPoints: points}
		case kindHistogram:
			points := make([]wireHistogramPoint, len(inst.series))
			for i, entry := range inst.series {
				counts := make([]string, len(entry.buckets))
				for b, count := range entry.buckets {
					counts[b] = formatUint(count)
				}
				minimum, maximum := entry.min, entry.max
				points[i] = wireHistogramPoint{Attributes: wireAttributes(entry.attributes), StartTimeUnixNano: start, TimeUnixNano: now, Count: formatUint(entry.count), Sum: entry.sum, BucketCounts: counts, ExplicitBounds: inst.bounds, Min: &minimum, Max: &maximum}
			}
			metric.Histogram = &wireHistogram{DataPoints: points, AggregationTemporality: aggregationCumulative}
		}
		metrics = append(metrics, metric)
	}
	request := wireMetricsRequest{ResourceMetrics: []wireResourceMetrics{{
		Resource:     wireResource{Attributes: wireAttributes(res.attributes)},
		ScopeMetrics: []wireScopeMetrics{{Scope: wireScope{Name: instrumentationScope, Version: res.version}, Metrics: metrics}},
	}}}
	return json.Marshal(request)
}

// metricExporter pushes metrics to the collector on an interval and once
// more at shutdown.
type metricExporter struct {
	interval  time.Duration
	export    func(context.Context) error
	health    *exportHealth
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	finalCtx  context.Context
	finalErr  error
}

func newMetricExporter(interval time.Duration, health *exportHealth, export func(context.Context) error) *metricExporter {
	exporter := &metricExporter{interval: interval, export: export, health: health, stop: make(chan struct{}), done: make(chan struct{})}
	go exporter.run()
	return exporter
}

func (e *metricExporter) run() {
	defer close(e.done)
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.health.record(e.export(context.Background()), 1)
		case <-e.stop:
			e.finalErr = e.export(e.finalCtx)
			e.health.record(e.finalErr, 1)
			return
		}
	}
}

func (e *metricExporter) shutdown(ctx context.Context) error {
	e.closeOnce.Do(func() {
		e.finalCtx = ctx
		close(e.stop)
	})
	select {
	case <-e.done:
		return e.finalErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
