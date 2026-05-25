package main

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// HTTP server metrics. The "route" label is the matched mux pattern (e.g.
// "GET /image"), not the raw path, so cardinality is bounded to the fixed set
// of registered routes; unmatched requests collapse into a single bucket.
var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkyframe_http_requests_total",
		Help: "Total HTTP requests, by method, matched route, and status code.",
	}, []string{"method", "route", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inkyframe_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds, by method and matched route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
)

// App image-build metrics. kind is "refresh" (synchronous, at startup) or
// "regenerate" (background); outcome is "success" or "error".
var (
	appBuildsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkyframe_app_builds_total",
		Help: "Total app image builds, by app, kind (refresh/regenerate), and outcome.",
	}, []string{"app", "kind", "outcome"})

	appBuildDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "inkyframe_app_build_duration_seconds",
		Help:    "App image build duration in seconds, by app and kind.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"app", "kind"})

	appLastBuildTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "inkyframe_app_last_successful_build_timestamp_seconds",
		Help: "Unix timestamp of the most recent successful image build, by app.",
	}, []string{"app"})

	appRegenSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "inkyframe_app_regenerations_skipped_total",
		Help: "Background regenerations skipped because one was already in flight, by app.",
	}, []string{"app"})
)

// recordBuild records the outcome and duration of one app image build. On
// success it also stamps the last-successful-build gauge so freshness can be
// alerted on (now - timestamp).
func recordBuild(app, kind string, err error, dur time.Duration) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	appBuildsTotal.WithLabelValues(app, kind, outcome).Inc()
	appBuildDuration.WithLabelValues(app, kind).Observe(dur.Seconds())
	if err == nil {
		appLastBuildTimestamp.WithLabelValues(app).SetToCurrentTime()
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code and the
// number of bytes written, for the access log and metrics. The status defaults
// to 200 because a handler that writes a body without calling WriteHeader
// implies a 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// instrument wraps the ServeMux to emit an access log line and record HTTP
// metrics for every request. It must wrap the mux (not individual handlers):
// once next.ServeHTTP returns, r.Pattern holds the route the mux matched
// (Go 1.23+), which is the bounded-cardinality "route" label. Scrapes of
// /metrics are passed through unlogged and uncounted to keep them out of the
// access log and out of their own series.
func instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		route := r.Pattern
		if route == "" {
			route = "<unmatched>"
		}
		httpRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		httpRequestDuration.WithLabelValues(r.Method, route).Observe(dur.Seconds())

		log.Printf("%s %s %s -> %d (%d bytes, %s)",
			r.RemoteAddr, r.Method, r.URL.RequestURI(), rec.status, rec.bytes, dur.Round(time.Millisecond))
	})
}
