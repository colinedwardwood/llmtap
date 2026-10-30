package proxy

import "net/http/httputil"

// ReverseProxiesForTest exposes the per-upstream ReverseProxy map so
// external tests can inspect the Transport configuration (TLS pinning,
// SNI, connection-pool tuning). Reserved for tests in the proxy_test
// package; not part of the public API.
func ReverseProxiesForTest(h *Handler) map[string]*httputil.ReverseProxy {
	return h.rps
}

// ActiveStreamsForTest returns the in-process active SSE stream count.
func ActiveStreamsForTest(h *Handler) int64 {
	if h == nil {
		return 0
	}
	return h.activeStreams.Load()
}
