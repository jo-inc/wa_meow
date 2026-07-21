package main

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	messagesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "jo_whatsapp_messages_total",
		Help: "Total WhatsApp messages processed",
	}, []string{"direction", "type"})

	messageProcessingSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "jo_whatsapp_message_processing_seconds",
		Help:    "Time spent processing messages",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"type"})

	mediaDownloadsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "jo_whatsapp_media_downloads_total",
		Help: "Total WhatsApp media download attempts",
	})

	mediaDownloadSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "jo_whatsapp_media_download_seconds",
		Help:    "Time spent downloading WhatsApp media",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 60},
	})

	mediaDownloadErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "jo_whatsapp_media_download_errors_total",
		Help: "Total WhatsApp media download failures",
	})

	activeSessions = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "jo_whatsapp_active_sessions",
		Help: "Current active WhatsApp sessions",
	})

	webhookRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "jo_whatsapp_webhook_requests_total",
		Help: "Total webhook requests processed",
	}, []string{"status"})

	apiErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "jo_whatsapp_api_errors_total",
		Help: "Total API errors by endpoint",
	}, []string{"endpoint"})

	sessionReconnectsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "jo_whatsapp_session_reconnects_total",
		Help: "Total WA session reconnection events",
	})

	sessionLifecycleTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "jo_whatsapp_session_lifecycle_total",
		Help: "WhatsApp session lifecycle transitions",
	}, []string{"transition"})

	activeSSEListeners = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "jo_whatsapp_sse_listeners",
		Help: "Current active SSE listeners across all sessions",
	})

	mediaCacheEntries = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "jo_whatsapp_media_cache_entries",
		Help: "Current entries across all session media caches",
	})

	mediaCacheBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "jo_whatsapp_media_cache_bytes",
		Help: "Current bytes across all session media caches",
	})

	mediaCacheEvictionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "jo_whatsapp_media_cache_evictions_total",
		Help: "Media cache evictions by reason",
	}, []string{"reason"})
)

func metricsHandler() http.Handler {
	return promhttp.Handler()
}

func instrumentHandler(endpoint string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw := &statusRecorder{ResponseWriter: w, status: 200}
		next(rw, r)
		if rw.status >= 400 {
			webhookRequestsTotal.WithLabelValues("error").Inc()
			apiErrorsTotal.WithLabelValues(endpoint).Inc()
		} else {
			webhookRequestsTotal.WithLabelValues("success").Inc()
		}
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func observeMessageProcessing(msgType string, start time.Time) {
	messageProcessingSeconds.WithLabelValues(msgType).Observe(time.Since(start).Seconds())
}
