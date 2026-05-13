package provider

import "testing"

// TestOperationForStrictMatching is the A22 regression. The previous
// implementation used strings.HasSuffix, so paths like
// /v1/files/chat/completions or /v1/anthropic/.well-known/messages
// matched and got enriched as if they were the real operation
// endpoints — feeding garbage attributes into spans and metrics, and
// occasionally tripping the body parsers on unexpected schemas.
//
// The strict rule: the segment immediately before the operation must
// look like an API version (`v\d+`, case-insensitive), OR — for the
// OpenAI Azure shape — the path must traverse `deployments/{name}`
// before the operation. Anything else returns "".
func TestOperationForStrictMatching(t *testing.T) {
	t.Parallel()

	type tc struct {
		name string
		path string
		want string
	}
	openaiCases := []tc{
		// Legitimate.
		{"chat v1", "/v1/chat/completions", "chat"},
		{"chat v2", "/v2/chat/completions", "chat"},
		{"chat tenanted v1", "/openai/v1/chat/completions", "chat"},
		{"chat azure deployment", "/openai/deployments/my-gpt-4o/chat/completions", "chat"},
		{"embeddings v1", "/v1/embeddings", "embeddings"},
		{"embeddings tenanted", "/openai/v1/embeddings", "embeddings"},
		// Impostors — suffix match used to (incorrectly) return non-empty.
		{"chat under files", "/v1/files/chat/completions", ""},
		{"chat trailing slash", "/v1/chat/completions/", ""},
		{"embeddings under files", "/v1/files/embeddings", ""},
		{"chat under non-version", "/foo/chat/completions", ""},
		{"completions only", "/v1/completions", ""},
		{"empty", "", ""},
		{"slash only", "/", ""},
		// Substring matches that suffix-matching never tripped on but
		// the strict rule must also reject.
		{"chat as substring of segment", "/v1/notchat/completions", ""},
		{"embeddings as substring of segment", "/v1/notembeddings", ""},
	}
	for _, c := range openaiCases {
		t.Run("openai/"+c.name, func(t *testing.T) {
			t.Parallel()
			if got := (OpenAI{}).OperationFor(c.path); got != c.want {
				t.Errorf("OpenAI.OperationFor(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}

	anthropicCases := []tc{
		// Legitimate.
		{"messages v1", "/v1/messages", "chat"},
		{"messages tenanted", "/anthropic/v1/messages", "chat"},
		// Impostors.
		{"messages under .well-known", "/v1/anthropic/.well-known/messages", ""},
		{"messages as substring", "/v1/messagesextra", ""},
		{"messages under files", "/v1/files/messages", ""},
		{"messages trailing slash", "/v1/messages/", ""},
		{"prefix only", "/v1/message", ""},
		{"empty", "", ""},
		{"slash only", "/", ""},
	}
	for _, c := range anthropicCases {
		t.Run("anthropic/"+c.name, func(t *testing.T) {
			t.Parallel()
			if got := (Anthropic{}).OperationFor(c.path); got != c.want {
				t.Errorf("Anthropic.OperationFor(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}
