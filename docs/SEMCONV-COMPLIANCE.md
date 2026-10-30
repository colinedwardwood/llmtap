# OpenTelemetry GenAI semantic-convention compliance

Snapshot: 2026-07. llmtap emits the OpenTelemetry v1.40 core schema URL from
the SDK instrumentation scope. GenAI development attributes track the separate
OpenTelemetry GenAI semantic-conventions repository; it does not yet publish a
dedicated schema URL. Fields marked extension are intentionally nonstandard.

| Signal | Status | Notes |
|---|---|---|
| `gen_ai.provider.name`, `gen_ai.operation.name` | emitted | OpenAI, Anthropic, Gemini |
| request/response model and response ID | emitted | when present on wire |
| request temperature, top-p/top-k, max tokens, stop, seed | emitted | provider-dependent |
| input/output token usage | emitted | spans and token histogram |
| cache-read/cache-creation/reasoning token breakdown | emitted | current dotted GenAI keys |
| response finish reasons | emitted | includes Responses status and Gemini reason |
| `error.type` | emitted | provider envelope or proxy classification |
| message/choice events | opt-in | `content.mode: events`, redacted |
| message/choice logs | opt-in | `content.mode: logs`, redacted |
| operation duration and token usage metrics | emitted | standard GenAI instruments |
| time to first token | extension | `gen_ai.client.time_to_first_token` |
| time to first byte | extension | `gen_ai.server.time_to_first_byte` |
| USD cost | extension | histogram, counter, span attribute |
| caller attribution | extension | `llmtap.caller`; capped on metrics |
| OpenAI request/response service tier | emitted | provider-specific GenAI keys |
| refusal content | opt-in | content event/log only; no invented span attribute |

Content is off by default. The W3C `baggage` propagator is deliberately not
enabled, and inbound `Baggage` is stripped before forwarding.
