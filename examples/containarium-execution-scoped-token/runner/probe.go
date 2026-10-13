// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import "strings"

// probeScript stands in for the agent. It is what ProcessService.Start runs
// inside the sandbox, and it is the only thing in this example that ever sees
// the run's token — handed to it in ProcessConfig.env_vars, and gone from the
// cluster the moment the process exits.
//
// Two things happen in here, in the order a reviewer should read them:
//
//  1. a model call THROUGH the gateway, authenticated with the token from the
//     environment. This is the intended path.
//  2. a direct TCP connect to the provider, which must fail. This is the path
//     the NetworkPolicy closes, and it is probed by IP, not by name, so that
//     a DNS failure can never be mistaken for policy enforcement.
//
// Written against bash's /dev/tcp rather than curl on purpose: the sandboxd
// image is debian-slim plus ca-certificates and procps — it ships no HTTP
// client. Rather than bake a new image (and give reviewers an unpinned
// dependency to audit) the probe uses what is already there. Real agent
// workloads bring their own HTTP client; nothing about the pattern depends on
// this choice.
//
// Each function body is a ( subshell ) so that a failed `exec` redirection
// terminates only the probe step, not the whole script — in a non-interactive
// shell a redirection error on `exec` is fatal to the shell itself.
const probeScript = `
set -u

echo "sandbox: run_id=${EST_RUN_ID}"
# Which EST_* variables this process received. NAMES ONLY — never echo a
# credential. The point of printing them is that the previous run's token is
# not among them: each Start gets its own env block, and nothing persists in
# the box between runs.
echo "sandbox: env keys from ProcessConfig.env_vars: $(env | sed -n 's/^\(EST_[A-Z_]*\)=.*/\1/p' | sort | tr '\n' ' ')"

# --- 1. the intended path: a model call through the credential proxy --------
gateway_post() (
  exec 3<>"/dev/tcp/${EST_GATEWAY_HOST}/${EST_GATEWAY_PORT}" || exit 1
  body='__MODEL_CALL_BODY__'
  printf 'POST __MODEL_CALL_PATH__ HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nanthropic-version: 2023-06-01\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' \
    "${EST_GATEWAY_HOST}" "${EST_GATEWAY_TOKEN}" "${#body}" "${body}" >&3
  # The gateway sets Connection: close, so this ends on its own; the timeout
  # is there so a wedged upstream cannot stall the run indefinitely.
  timeout 45 cat <&3
)

echo "__GATEWAY_BEGIN__"
gateway_post || echo "probe: gateway call failed to complete"
echo ""
echo "__GATEWAY_END__"

# --- 2. the path the NetworkPolicy must close ------------------------------
# Probed by IP (resolved outside the box and passed in), so a DNS failure can
# never be mistaken for policy enforcement.
#
# Both probes go through one helper, and the helper is exercised against a
# target the policy ALLOWS immediately before the one it must block. That
# positive control is not decoration: a "blocked" verdict produced by a typo in
# the probe itself would be indistinguishable from a real drop, and a check
# that cannot fail proves nothing. The runner requires "control rc=0" alongside
# the blocked verdict before it will call check 2 a pass.
TCP_PROBE='exec 3<>"/dev/tcp/$1/$2"'
probe_tcp() {
  local rc=0
  timeout 10 bash -c "${TCP_PROBE}" tcp-probe "$1" "$2" 2>/dev/null || rc=$?
  printf '%s' "${rc}"
}

echo "__EGRESS_BEGIN__"
control_rc="$(probe_tcp "${EST_GATEWAY_HOST}" "${EST_GATEWAY_PORT}")"
echo "control: TCP connect to the ALLOWED gateway ${EST_GATEWAY_HOST}:${EST_GATEWAY_PORT} rc=${control_rc}"
echo "probe:   direct TCP connect to ${EST_PROVIDER_IP}:${EST_PROVIDER_PORT} (${EST_PROVIDER_HOST})"
egress_rc="$(probe_tcp "${EST_PROVIDER_IP}" "${EST_PROVIDER_PORT}")"
if [ "${egress_rc}" -eq 0 ]; then
  echo "direct-connect: ESTABLISHED rc=${egress_rc} (control rc=${control_rc})"
else
  echo "direct-connect: FAILED rc=${egress_rc} (control rc=${control_rc})"
fi
echo "__EGRESS_END__"
`

// Markers the runner slices the probe's stdout on. Chosen so they cannot
// collide with anything in an HTTP response body.
const (
	gatewayBegin = "__GATEWAY_BEGIN__"
	gatewayEnd   = "__GATEWAY_END__"
	egressBegin  = "__EGRESS_BEGIN__"
	egressEnd    = "__EGRESS_END__"
)

// renderProbe substitutes the request the probe should make, so the in-sandbox
// call and the runner's own post-exit call (gateway.go) cannot drift apart.
func renderProbe() string {
	s := strings.ReplaceAll(probeScript, "__MODEL_CALL_BODY__", modelCallBody)
	return strings.ReplaceAll(s, "__MODEL_CALL_PATH__", modelCallPath)
}

// section returns the text between two markers, or "" if the pair is absent.
func section(out, begin, end string) string {
	_, rest, ok := strings.Cut(out, begin)
	if !ok {
		return ""
	}
	body, _, ok := strings.Cut(rest, end)
	if !ok {
		return ""
	}
	return strings.TrimSpace(body)
}

// parseHTTPResponse pulls the status code and body out of the raw HTTP/1.1
// response the probe printed. Deliberately minimal — it only has to read
// responses from one gateway, and hand-parsing keeps the evidence chain from
// "bytes on the wire inside the sandbox" to "this check passed" short enough
// to audit.
func parseHTTPResponse(raw string) (httpResult, bool) {
	raw = strings.TrimLeft(raw, "\r\n")
	line, rest, ok := strings.Cut(raw, "\n")
	if !ok {
		return httpResult{}, false
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return httpResult{}, false
	}
	status := 0
	for _, c := range fields[1] {
		if c < '0' || c > '9' {
			return httpResult{}, false
		}
		status = status*10 + int(c-'0')
	}
	// Headers end at the first blank line; everything after is the body. A
	// response with no such terminator (truncated, or otherwise malformed)
	// is not parseable — treating the whole remainder as both headers AND
	// body, as an earlier version of this function did, risks reading
	// something that happens to look like a header out of what is really
	// body content instead of failing loudly.
	var headers, body string
	if before, after, found := strings.Cut(rest, "\r\n\r\n"); found {
		headers, body = before, after
	} else if before, after, found := strings.Cut(rest, "\n\n"); found {
		headers, body = before, after
	} else {
		return httpResult{}, false
	}
	return httpResult{
		Status:      status,
		Body:        strings.TrimSpace(body),
		ContentType: headerValue(headers, "Content-Type"),
	}, true
}

// headerValue returns the value of the first header named name (matched
// case-insensitively, per RFC 9110) in a raw CRLF- or LF-separated header
// block, or "" if absent.
func headerValue(headers, name string) string {
	for line := range strings.SplitSeq(strings.ReplaceAll(headers, "\r\n", "\n"), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
