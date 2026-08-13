#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Verification script for the envd-sandbox example.
# Drives the envd REST API using curl + jq.
#
# Run:
#   SANDBOX_BASE_URL=http://127.0.0.1:49983 ./test_client.sh

set -euo pipefail

BASE_URL="${SANDBOX_BASE_URL:?Set SANDBOX_BASE_URL, e.g. http://127.0.0.1:49983}"

PASS=0
FAIL=0

run_test() {
  local name="$1"
  shift
  printf "  running %s... " "$name"
  if output=$("$@" 2>&1); then
    printf "PASS: %s\n" "$output"
    PASS=$((PASS + 1))
  else
    printf "FAIL: %s\n" "$output"
    FAIL=$((FAIL + 1))
  fi
}

echo ""
echo "=== envd-sandbox verification: curl ==="
echo ""

# 1. Health check
run_test "health" bash -c "
  code=\$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 10 '${BASE_URL}/health')
  [[ \"\$code\" == '204' ]] || { echo \"expected 204, got \$code\"; exit 1; }
  echo '204 No Content'
"

# 2. Init
run_test "init" bash -c "
  code=\$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 10 -X POST '${BASE_URL}/init' \
    -H 'Content-Type: application/json' \
    -d '{\"envVars\":{\"HELLO\":\"envd\"},\"defaultUser\":\"user\"}')
  [[ \"\$code\" == '204' ]] || { echo \"expected 204, got \$code\"; exit 1; }
  echo 'init ok'
"

# 3. File upload + download round-trip
run_test "files" bash -c "
  echo -n 'hi from envd-sandbox' > /tmp/envd-test-hello.txt
  trap 'rm -f /tmp/envd-test-hello.txt' EXIT
  code=\$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 10 -X POST '${BASE_URL}/files' \
    -F 'path=hello.txt' \
    -F 'file=@/tmp/envd-test-hello.txt')
  [[ \"\$code\" == '200' ]] || { echo \"upload: expected 200, got \$code\"; exit 1; }
  content=\$(curl -s --connect-timeout 5 --max-time 10 '${BASE_URL}/files?path=hello.txt')
  [[ \"\$content\" == 'hi from envd-sandbox' ]] || { echo \"content mismatch: \$content\"; exit 1; }
  echo 'round-trip ok'
"

# 4. Metrics
run_test "metrics" bash -c "
  resp=\$(curl -s --connect-timeout 5 --max-time 10 '${BASE_URL}/metrics')
  echo \"\$resp\" | jq -e 'has(\"ts\") or has(\"cpu_count\")' > /dev/null \
    || { echo \"unexpected metrics response: \$resp\"; exit 1; }
  echo \"keys: \$(echo \"\$resp\" | jq -r 'keys | join(\",\")')\"
"

echo ""
echo "=== Summary ==="
echo "  PASS: $PASS"
echo "  FAIL: $FAIL"
echo ""

if [[ "$FAIL" -gt 0 ]]; then
  echo "$FAIL test(s) failed."
  exit 1
fi

echo "All $PASS test(s) passed."
exit 0
