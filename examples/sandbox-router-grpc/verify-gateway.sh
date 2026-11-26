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

# Explicit TLS Gateway lane: assumes the dedicated deployment in README.md.
# This script issues RPCs only; it neither installs nor deletes cluster resources.
set -euo pipefail
: "${ADDRESS:?set ADDRESS to the TLS Gateway host:port}"
: "${CA_FILE:?set CA_FILE to the trusted CA certificate}"
: "${TOKEN_FILE:?set TOKEN_FILE to the scoped bearer token file}"
demo_bin="${DEMO_BIN:-./bin/sandbox-router-grpc}"
test_dir="$(mktemp -d)"
cancel_pid=""
cleanup() {
  if [[ -n "$cancel_pid" ]]; then
    kill "$cancel_pid" 2>/dev/null || true
    wait "$cancel_pid" 2>/dev/null || true
  fi
  rm -f "$test_dir/out" "$test_dir/err" "$test_dir/wrong-ca.crt" "$test_dir/wrong-ca.key"
  rmdir "$test_dir"
}
trap cleanup EXIT
common=(--address "$ADDRESS" --namespace "${NAMESPACE:-grpc-router-demo}" --sandbox "${SANDBOX:-box-a}"
  --port 9090 --ca-file "$CA_FILE" --server-name "${SERVER_NAME:-grpc-router.local}" --token-file "$TOKEN_FILE")
expect_exit() {
  local expected="$1" actual
  shift
  if "$demo_bin" "${common[@]}" "$@" >"$test_dir/out" 2>"$test_dir/err"; then
    actual=0
  else
    actual=$?
  fi
  if [[ "$actual" != "$expected" ]]; then
    printf 'expected exit %s, got %s\n' "$expected" "$actual" >&2
    # The example prints output/status only, never credentials.
    cat "$test_dir/out" "$test_dir/err" >&2
    exit 1
  fi
}
expect_exit 7 --mode execute --timeout 30s -- /bin/sh -c 'printf hello; printf warning >&2; exit 7'
[[ "$(<"$test_dir/out")" == hello ]]
grep -q warning "$test_dir/err"
grep -q 'exit_code=7' "$test_dir/err"
expect_exit 0 --mode interact --timeout 30s --stdin gateway
grep -q 'input:gateway' "$test_dir/out"
# A killed process is a successful RPC with a nonzero business exit code.
if "$demo_bin" "${common[@]}" --mode signal --timeout 30s >"$test_dir/out" 2>"$test_dir/err"; then
  printf 'SendSignal unexpectedly produced exit_code=0\n' >&2
  exit 1
fi
grep -q ready "$test_dir/out"
grep -q 'exit_code=' "$test_dir/err"

# Both late unary headers and long streaming must outlive Gateway defaults.
expect_exit 0 --mode execute --timeout 30s -- /bin/sh -c 'sleep 20; printf late'
[[ "$(<"$test_dir/out")" == late ]]
started=$SECONDS
# Expand these loop variables inside the sandbox, not in the wrapper.
# shellcheck disable=SC2016
expect_exit 0 --mode start --timeout 60s -- /bin/sh -c 'i=0; while [ "$i" -lt 45 ]; do printf "tick\n"; sleep 1; i=$((i+1)); done'
[[ "$(grep -c '^tick$' "$test_dir/out")" == 45 ]]
[[ $((SECONDS - started)) -ge 40 ]]
expect_exit 1 --mode execute --timeout 500ms -- /bin/sh -c 'sleep 300'
grep -q 'code = DeadlineExceeded' "$test_dir/err"

# Cancel after receiving actual process output, not after guessing startup time.
"$demo_bin" "${common[@]}" --mode start --timeout 30s -- /bin/sh -c 'printf "ready\n"; sleep 300' >"$test_dir/out" 2>"$test_dir/err" &
cancel_pid=$!
for ((attempt = 0; attempt < 20; attempt++)); do
  if grep -q ready "$test_dir/out"; then break; fi
  sleep 1
done
grep -q ready "$test_dir/out"
kill -INT "$cancel_pid"
if wait "$cancel_pid"; then
  printf 'canceled RPC unexpectedly succeeded\n' >&2
  exit 1
fi
cancel_pid=""
grep -q 'code = Canceled' "$test_dir/err"
expect_exit 0 --mode execute --timeout 30s -- /bin/true

expect_exit 1 --sandbox '' --mode execute --timeout 5s
grep -q 'code = InvalidArgument' "$test_dir/err"
expect_exit 1 --token-file '' --mode execute --timeout 5s
grep -q 'code = Unauthenticated' "$test_dir/err"
expect_exit 1 --sandbox another-sandbox --mode execute --timeout 5s
grep -q 'code = PermissionDenied' "$test_dir/err"
expect_exit 1 --mode execute --timeout 5s -- /command-does-not-exist
grep -q 'code = NotFound' "$test_dir/err"
expect_exit 1 --server-name wrong.example.invalid --mode execute --timeout 2s
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=untrusted-test-ca \
  -keyout "$test_dir/wrong-ca.key" -out "$test_dir/wrong-ca.crt" >/dev/null 2>&1
expect_exit 1 --ca-file "$test_dir/wrong-ca.crt" --mode execute --timeout 2s
printf 'TLS Gateway gRPC checks passed (including the 45-second stream).\n'
