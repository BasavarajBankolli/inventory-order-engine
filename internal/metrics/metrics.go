// Package metrics defines the service's Prometheus metrics.
//
// Metrics answer "how is the system doing RIGHT NOW, and how did it do over
// time?" - requests per second, error rate, latency, orders per minute,
// payment failures. Logs tell you about ONE request; metrics about all of
// them. Prometheus scrapes GET /metrics every few seconds and stores the
// numbers; Grafana draws them; alerts fire on thresholds.
//
// Design rules used here:
//   - One Metrics value per process, passed in (dependency injection) - no
//     global default registry, so tests can create their own.
//   - Methods are nil-safe: code built without metrics (many tests) can
//     pass a nil *Metrics and every call is a no-op.
//   - Labels have a SMALL, fixed set of values (route patterns, reasons,
//     status codes). Never put ids, emails or raw URLs in labels: every
//     distinct label value creates a new time series ("cardinality
//     explosion"), which can take a monitoring system down.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every metric of this service.
type Metrics struct {
	registry *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec

	ordersCreated        prometheus.Counter
	ordersFailed         *prometheus.CounterVec
	inventoryReservation *prometheus.CounterVec
	paymentsSuccess      prometheus.Counter
	paymentsFailed       *prometheus.CounterVec
	outboxProcessed      *prometheus.CounterVec
	productCache         *prometheus.CounterVec
	rateLimited          prometheus.Counter
}

// New creates and registers all metrics, plus Go runtime and process
// metrics (goroutines, memory, GC, CPU, open file descriptors).
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),

		// COUNTER: a number that only goes up. Prometheus computes rates
		// from it, e.g. rate(http_requests_total[5m]) = requests/second.
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests handled, by method, route pattern and status code.",
		}, []string{"method", "route", "status"}),

		// HISTOGRAM: counts observations in buckets, so Prometheus can
		// compute percentiles, e.g. the p95 latency with
		// histogram_quantile(0.95, rate(http_request_duration_seconds_bucket[5m])).
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency, by method and route pattern.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),

		ordersCreated: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "orders_created_total",
			Help: "Orders successfully created (stock reserved, transaction committed).",
		}),
		ordersFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "orders_failed_total",
			Help: "Order creations that failed, by reason.",
		}, []string{"reason"}),
		inventoryReservation: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inventory_reservations_total",
			Help: "Reservation lifecycle events (one per order line): created, confirmed, released, expired.",
		}, []string{"event"}),
		paymentsSuccess: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "payments_success_total",
			Help: "Payments that succeeded (order confirmed).",
		}),
		paymentsFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "payments_failed_total",
			Help: "Payments that did not succeed, by reason: declined, timeout (outcome unknown).",
		}, []string{"reason"}),
		outboxProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_events_processed_total",
			Help: "Outbox publish attempts, by result: published, retry, dead.",
		}, []string{"result"}),
		productCache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "product_cache_requests_total",
			Help: "Product cache lookups, by result: hit, miss, error.",
		}, []string{"result"}),
		rateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "http_rate_limited_total",
			Help: "Requests rejected with 429 by the rate limiter.",
		}),
	}

	m.registry.MustRegister(
		m.httpRequests, m.httpDuration,
		m.ordersCreated, m.ordersFailed, m.inventoryReservation,
		m.paymentsSuccess, m.paymentsFailed, m.outboxProcessed,
		m.productCache, m.rateLimited,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the metrics in Prometheus text format (GET /metrics).
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry exposes the registry (tests read values from it).
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Value returns the current value of a counter (or a histogram's sample
// count) with exactly the given labels, given as name/value pairs:
//
//	m.Value("orders_failed_total", "reason", "out_of_stock")
//
// It returns 0 if that series does not exist yet. Used by tests in other
// packages to assert "this action incremented that metric".
func (m *Metrics) Value(name string, labelPairs ...string) float64 {
	families, err := m.registry.Gather()
	if err != nil {
		return 0
	}
	want := map[string]string{}
	for i := 0; i+1 < len(labelPairs); i += 2 {
		want[labelPairs[i]] = labelPairs[i+1]
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	series:
		for _, s := range f.GetMetric() {
			if len(s.GetLabel()) != len(want) {
				continue
			}
			for _, l := range s.GetLabel() {
				if want[l.GetName()] != l.GetValue() {
					continue series
				}
			}
			if s.Counter != nil {
				return s.GetCounter().GetValue()
			}
			if s.Histogram != nil {
				return float64(s.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

// --- recording methods (all nil-safe) ------------------------------------------

// ObserveHTTP records one finished request.
func (m *Metrics) ObserveHTTP(method, route string, status int, seconds float64) {
	if m == nil {
		return
	}
	m.httpRequests.WithLabelValues(method, route, statusLabel(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(seconds)
}

// OrderCreated records a committed order with `lines` reserved order lines.
func (m *Metrics) OrderCreated(lines int) {
	if m == nil {
		return
	}
	m.ordersCreated.Inc()
	m.inventoryReservation.WithLabelValues("created").Add(float64(lines))
}

// OrderFailed records a failed order creation. reason must come from a
// small fixed set (see orders.failureReason).
func (m *Metrics) OrderFailed(reason string) {
	if m == nil {
		return
	}
	m.ordersFailed.WithLabelValues(reason).Inc()
}

// ReservationsFinished records reservations leaving ACTIVE:
// event is "confirmed", "released" or "expired".
func (m *Metrics) ReservationsFinished(event string, n int) {
	if m == nil || n == 0 {
		return
	}
	m.inventoryReservation.WithLabelValues(event).Add(float64(n))
}

// PaymentSucceeded records a successful payment.
func (m *Metrics) PaymentSucceeded() {
	if m == nil {
		return
	}
	m.paymentsSuccess.Inc()
}

// PaymentFailed records a failed payment: reason "declined" or "timeout".
func (m *Metrics) PaymentFailed(reason string) {
	if m == nil {
		return
	}
	m.paymentsFailed.WithLabelValues(reason).Inc()
}

// OutboxProcessed records outbox publish results.
func (m *Metrics) OutboxProcessed(published, retry, dead int) {
	if m == nil {
		return
	}
	m.outboxProcessed.WithLabelValues("published").Add(float64(published))
	m.outboxProcessed.WithLabelValues("retry").Add(float64(retry))
	m.outboxProcessed.WithLabelValues("dead").Add(float64(dead))
}

// ProductCache records a cache lookup: "hit", "miss" or "error".
func (m *Metrics) ProductCache(result string) {
	if m == nil {
		return
	}
	m.productCache.WithLabelValues(result).Inc()
}

// RateLimited records a 429.
func (m *Metrics) RateLimited() {
	if m == nil {
		return
	}
	m.rateLimited.Inc()
}

// statusLabel uses the numeric code ("200", "409"): the Prometheus
// convention, so alerts can match whole classes with status=~"5..".
// There are only a few dozen possible values, so cardinality stays small.
func statusLabel(code int) string {
	return strconv.Itoa(code)
}
