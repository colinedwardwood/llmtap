#!/usr/bin/env bash
# End-to-end compose smoke test (D12): brings up the demo stack with a
# stub upstream, sends one chat completion through llmtap, and asserts
# that token and cost series actually land in Prometheus. Proves the
# README demo pipeline (proxy -> collector -> prometheus -> dashboard)
# end to end without any provider credentials.
set -euo pipefail

cd "$(dirname "$0")/../.."

COMPOSE=(docker compose
  -f deploy/compose/docker-compose.yml
  -f deploy/compose/docker-compose.smoke.yml)

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "--- smoke test FAILED (exit $status); service logs follow ---"
    "${COMPOSE[@]}" logs --no-color llmtap stub otel-collector prometheus || true
  fi
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT

echo "==> bringing up the stack"
"${COMPOSE[@]}" up -d --build

echo "==> waiting for llmtap /healthz"
for i in $(seq 1 60); do
  if curl -fsS http://localhost:4000/healthz >/dev/null 2>&1; then
    break
  fi
  [ "$i" -eq 60 ] && { echo "llmtap never became healthy"; exit 1; }
  sleep 1
done

echo "==> sending a chat completion through the proxy"
response=$(curl -fsS http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer smoke-test-not-a-real-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-mini","messages":[{"role":"user","content":"smoke"}]}')
echo "$response" | grep -q '"chatcmpl-smoke"' || {
  echo "unexpected proxy response: $response"; exit 1;
}

# The metric reader exports every 15s and the collector batches, so
# give the pipeline up to 2 minutes before declaring failure.
assert_series() {
  local metric=$1
  echo "==> waiting for non-empty series: $metric"
  for i in $(seq 1 60); do
    local result
    result=$(curl -fsS "http://localhost:9090/api/v1/query" \
      --data-urlencode "query=${metric}" | python3 -c \
      'import json,sys; print(len(json.load(sys.stdin)["data"]["result"]))')
    if [ "$result" -gt 0 ]; then
      echo "    OK ($result series)"
      return 0
    fi
    sleep 2
  done
  echo "no series found for $metric within 120s"
  return 1
}

assert_series 'gen_ai_client_token_usage_sum'
assert_series 'gen_ai_client_cost_usd_total'

echo "==> smoke test PASSED"
