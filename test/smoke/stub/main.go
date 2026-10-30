// Command stub is a minimal OpenAI-compatible upstream for the compose
// smoke test. It answers POST /v1/chat/completions with a fixed,
// well-formed non-streaming response that carries token usage, so
// llmtap's enrichment path produces token and cost metrics without any
// real provider credentials.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the connection is reusable.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-smoke",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-5-mini",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "hello from the smoke stub",
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     12,
				"completion_tokens": 7,
				"total_tokens":      19,
			},
		})
	})
	log.Println("stub upstream listening on :9999")
	log.Fatal(http.ListenAndServe(":9999", mux))
}
