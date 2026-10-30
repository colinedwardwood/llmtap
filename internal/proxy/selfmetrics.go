package proxy

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Rejection-reason label values for llmtap.requests.rejected.total.
// Keep the cardinality bounded and the spelling stable — dashboards
// and alert rules key off these exact strings.
const (
	rejectReasonAuth         = "auth"
	rejectReasonAuthBusy     = "auth_busy"
	rejectReasonNotFound     = "not_found"
	rejectReasonBreakerOpen  = "breaker_open"
	rejectReasonConcurrency  = "concurrency"
	rejectReasonBodyTooLarge = "body_too_large"
)

// Breaker gauge encodings. Documented so PromQL / alerts stay stable:
//
//	0 = closed, 1 = open, 2 = half_open
const (
	breakerGaugeClosed   int64 = 0
	breakerGaugeOpen     int64 = 1
	breakerGaugeHalfOpen int64 = 2
)

func breakerStateGauge(s breakerState) int64 {
	switch s {
	case breakerOpen:
		return breakerGaugeOpen
	case breakerHalfOpen:
		return breakerGaugeHalfOpen
	default:
		return breakerGaugeClosed
	}
}

func (h *Handler) recordRequest(ctx context.Context, upstream string, status int) {
	if status == 0 {
		status = 502
	}
	if upstream == "" {
		upstream = "_none"
	}
	h.requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("upstream", upstream),
		attribute.Int("http.response.status_code", status),
	))
}

func (h *Handler) recordRejected(ctx context.Context, reason, upstream string) {
	if upstream == "" {
		upstream = "_none"
	}
	h.rejected.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", reason),
		attribute.String("upstream", upstream),
	))
}

func (h *Handler) setBreakerState(upstream string, s breakerState) {
	h.breakerState.Record(context.Background(), breakerStateGauge(s),
		metric.WithAttributes(attribute.String("upstream", upstream)))
}

func (h *Handler) incActiveStreams(delta int64) {
	h.activeStreams.Add(delta)
	h.streamsActive.Add(context.Background(), delta)
}

func (h *Handler) recordStreamIdleTimeout(upstream string) {
	if h.streamIdleTimeouts == nil {
		return
	}
	if upstream == "" {
		upstream = "_none"
	}
	h.streamIdleTimeouts.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("upstream", upstream)))
}

// certNearExpiryWarn is the lead time for the boot-time warning that
// the served leaf is about to expire. Long enough for cert-manager
// renewals to be investigated before handshakes fail.
const certNearExpiryWarn = 14 * 24 * time.Hour

// observeCert records the leaf NotAfter on the gauge and warns when
// expiry is inside certNearExpiryWarn. Called at boot and after every
// successful reload.
func (h *Handler) observeCert(notAfter time.Time, logger *slog.Logger) {
	if h.certNotAfter == nil {
		return
	}
	var unix int64
	if !notAfter.IsZero() {
		unix = notAfter.Unix()
	}
	h.certNotAfter.Record(context.Background(), unix)
	if logger != nil && !notAfter.IsZero() {
		until := time.Until(notAfter)
		if until > 0 && until < certNearExpiryWarn {
			logger.Warn("tls certificate expires soon",
				slog.Time("not_after", notAfter),
				slog.Duration("remaining", until.Round(time.Hour)),
			)
		}
	}
}

func (h *Handler) recordCertReloadFailure() {
	if h.certReloadFailures != nil {
		h.certReloadFailures.Add(context.Background(), 1)
	}
}
