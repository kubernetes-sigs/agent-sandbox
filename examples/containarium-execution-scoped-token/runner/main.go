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

// Runner for the execution-scoped-token example: it gives one run of an agent
// process exactly one credential, scoped to that run, and takes it back when
// the process exits.
//
// Per run, in order:
//
//  1. mint a run-scoped gateway token, keeping only its jti;
//  2. ProcessService.Start the process with the token in
//     ProcessConfig.env_vars and nowhere else;
//  3. read the stream through to ExitEvent;
//  4. revoke the jti at the gateway.
//
// Then it proves the four properties the README lists, printing each with the
// status code and body it actually observed.
//
// # Why the generated processv1 stubs and not clients/go/sandbox
//
// The high-level Go client is the better tool for almost everything, but its
// only hook for environment variables is Options.Env, which injects them into
// the SandboxClaim — so the token would sit in the pod spec for the life of
// the sandbox, readable by anyone with `get pod`, inherited by every later run
// in the same box. That is precisely the anti-pattern this example exists to
// replace, so the run goes through ProcessService directly, where env_vars
// belong to one process invocation and expire with it.
//
// The high-level client remains a fine way to create and delete the Sandbox
// claim itself; this example uses a plain manifest (sandbox.yaml) for that,
// matching the other manifest-driven examples in this directory. A `WithEnv`
// call option on Commands.Run would let the high-level client express this
// pattern too — see the README's "Suggested follow-up upstream".
//
// Usage (run-test-kind.sh sets the port-forwards up and passes these in):
//
//	go run ./examples/containarium-execution-scoped-token/runner \
//	  -sandboxd-addr 127.0.0.1:9090 \
//	  -gateway-url http://127.0.0.1:8866 \
//	  -gateway-in-cluster-host model-gateway.default.svc.cluster.local \
//	  -secret-file /path/to/jwt.secret \
//	  -admin-token-file /path/to/gateway-admin.token \
//	  -provider-addr 160.79.104.10:443
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

type options struct {
	sandboxdAddr         string
	gatewayURL           string
	gatewayInClusterHost string
	gatewayInClusterPort string
	secretFile           string
	adminTokenFile       string
	providerAddr         string
	providerHost         string
	tenant               string
	skill                string
	tokenTTL             time.Duration

	// providerAddr, split. The probe needs them separately because bash's
	// /dev/tcp takes /dev/tcp/<host>/<port>, not host:port.
	providerIP   string
	providerPort string
}

func main() {
	var o options
	flag.StringVar(&o.sandboxdAddr, "sandboxd-addr", "127.0.0.1:9090",
		"sandboxd ProcessService gRPC address, as reached from here (a kubectl port-forward).")
	flag.StringVar(&o.gatewayURL, "gateway-url", "http://127.0.0.1:8866",
		"Model gateway base URL as reached from here (a kubectl port-forward). Used for revoke and for the post-exit checks.")
	flag.StringVar(&o.gatewayInClusterHost, "gateway-in-cluster-host", "model-gateway.default.svc.cluster.local",
		"Model gateway host as reached from INSIDE the sandbox. This is the only egress the NetworkPolicy allows.")
	flag.StringVar(&o.gatewayInClusterPort, "gateway-in-cluster-port", "8866",
		"Model gateway port as reached from inside the sandbox.")
	flag.StringVar(&o.secretFile, "secret-file", "",
		"File holding the HMAC secret the gateway verifies tokens with (required).")
	flag.StringVar(&o.adminTokenFile, "admin-token-file", "",
		"File holding the gateway's admin bearer token, for POST /__gateway/revoke (required).")
	flag.StringVar(&o.providerAddr, "provider-addr", "",
		"host:port the in-sandbox probe attempts a DIRECT connection to, by IP, to show the NetworkPolicy blocks it (required).")
	flag.StringVar(&o.providerHost, "provider-host", "api.anthropic.com",
		"Name that -provider-addr was resolved from. Reporting only.")
	flag.StringVar(&o.tenant, "tenant", "example-tenant", "Tenant the token is scoped to.")
	flag.StringVar(&o.skill, "skill", "execution-scoped-demo", "Skill/agent id the token is scoped to.")
	flag.DurationVar(&o.tokenTTL, "token-ttl", 30*time.Minute,
		"Token lifetime. Deliberately long relative to the run: the point is that revocation, not expiry, is what ends the credential.")
	flag.Parse()

	for name, v := range map[string]string{
		"-secret-file":      o.secretFile,
		"-admin-token-file": o.adminTokenFile,
		"-provider-addr":    o.providerAddr,
	} {
		if v == "" {
			log.Fatalf("%s is required", name)
		}
	}

	var err error
	if o.providerIP, o.providerPort, err = net.SplitHostPort(o.providerAddr); err != nil {
		log.Fatalf("-provider-addr %q: %v", o.providerAddr, err)
	}
	// An IP, not a name, on purpose: resolving inside the box would let a DNS
	// failure masquerade as a policy drop.
	if net.ParseIP(o.providerIP) == nil {
		log.Fatalf("-provider-addr %q: %q is not an IP address; resolve the provider "+
			"on the host and pass the address, so the egress check cannot pass because DNS failed",
			o.providerAddr, o.providerIP)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, o); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
}

// runRecord is what one completed run leaves behind: the credential it was
// given, and what the box observed while holding it.
type runRecord struct {
	token   mintedToken
	gateway httpResult // the model call the process made from inside the box
	egress  string     // the direct-egress probe's verdict line
}

func run(ctx context.Context, o options) error {
	secret, err := readTrimmed(o.secretFile)
	if err != nil {
		return err
	}
	adminToken, err := readTrimmed(o.adminTokenFile)
	if err != nil {
		return err
	}

	conn, err := grpc.NewClient(o.sandboxdAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial sandboxd at %s: %w", o.sandboxdAddr, err)
	}
	defer conn.Close()
	proc := processv1.NewProcessServiceClient(conn)

	var records []runRecord
	for i := 1; i <= 2; i++ {
		fmt.Printf("\n=== run %d ===\n", i)
		rec, err := doRun(ctx, o, proc, string(secret), string(adminToken))
		if err != nil {
			return fmt.Errorf("run %d: %w", i, err)
		}
		records = append(records, rec)

		// Between the two runs, prove that run 1's credential is dead while
		// run 2 is about to get a working one. Checked here, not after the
		// loop, so the assertion is made while the box is demonstrably still
		// alive and serving — "the token died, not the sandbox".
		if i == 1 {
			continue
		}
		for j, prev := range records[:len(records)-1] {
			res, err := callModelThroughGateway(ctx, o.gatewayURL, prev.token.Token)
			if err != nil {
				return fmt.Errorf("re-checking run %d's token: %w", j+1, err)
			}
			if refusal := gatewayRefusal(res); refusal != "gateway token revoked" {
				return fmt.Errorf("run %d's token should still be revoked, got %s", j+1, res.summary())
			}
			fmt.Printf("run %d token (jti %s) is still dead: %s\n", j+1, prev.token.JTI, res.summary())
		}
	}

	return report(o, records)
}

// doRun is the lease: mint, start, wait for exit, revoke. The revoke is
// deferred so that every exit path from this function ends the credential —
// including the error paths, which is the half that gets forgotten. Named
// returns so the deferred revoke can fold a revoke failure into the
// function's own error: a run whose credential silently failed to revoke is
// not a run that succeeded, and this example's entire point is that the
// revoke actually happens.
func doRun(ctx context.Context, o options, proc processv1.ProcessServiceClient, secret, adminToken string) (rec runRecord, err error) {
	runID, err := newRunID()
	if err != nil {
		return runRecord{}, err
	}
	tok, err := mintGatewayToken([]byte(secret), o.tenant, o.skill, runID, o.tokenTTL)
	if err != nil {
		return runRecord{}, fmt.Errorf("mint: %w", err)
	}
	fmt.Printf("minted a token for run_id=%s (jti=%s, expires %s — %s from now)\n",
		tok.RunID, tok.JTI, tok.ExpiresAt.UTC().Format(time.RFC3339), o.tokenTTL)

	rec = runRecord{token: tok}
	defer func() {
		// Detached from ctx on purpose: a cancelled or timed-out run must
		// still give its credential back. In Containarium's daemon this is
		// runlease.End under context.WithoutCancel for exactly this reason.
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if revokeErr := revokeToken(revokeCtx, o.gatewayURL, adminToken, tok, "run_exit"); revokeErr != nil {
			fmt.Fprintf(os.Stderr, "ERROR: revoking run %s failed: %v\n", tok.RunID, revokeErr)
			err = errors.Join(err, fmt.Errorf("revoke run %s (jti %s): %w", tok.RunID, tok.JTI, revokeErr))
			return
		}
		fmt.Printf("run %s exited; revoked jti %s at the gateway\n", tok.RunID, tok.JTI)
	}()

	out, exitCode, err := startAndWait(ctx, proc, &processv1.ProcessConfig{
		Command: []string{"bash", "-c", renderProbe()},
		// THE POINT OF THE EXAMPLE. The token is handed to this one process
		// invocation and lives only in its environment: not in the Sandbox
		// spec, not in the SandboxClaim, not in a Secret, not on the
		// workspace volume, and not in the next run's environment.
		EnvVars: map[string]string{
			"EST_RUN_ID":        tok.RunID,
			"EST_GATEWAY_TOKEN": tok.Token,
			"EST_GATEWAY_HOST":  o.gatewayInClusterHost,
			"EST_GATEWAY_PORT":  o.gatewayInClusterPort,
			"EST_PROVIDER_IP":   o.providerIP,
			"EST_PROVIDER_PORT": o.providerPort,
			"EST_PROVIDER_HOST": o.providerHost,
		},
	})
	if err != nil {
		return rec, err
	}
	fmt.Printf("--- process output (exit=%d) ---\n%s\n--- end process output ---\n",
		exitCode, strings.TrimSpace(out))
	// A non-zero exit is a failed run even if the probe already printed
	// parseable sections before failing — the markers below only prove what
	// ran before whatever killed the process, not that the run succeeded.
	if exitCode != 0 {
		return rec, fmt.Errorf("process exited %d (see output above)", exitCode)
	}

	res, ok := parseHTTPResponse(section(out, gatewayBegin, gatewayEnd))
	if !ok {
		return rec, fmt.Errorf("could not parse the in-sandbox gateway response out of the process output")
	}
	rec.gateway = res
	rec.egress = lastLine(section(out, egressBegin, egressEnd))
	if rec.egress == "" {
		return rec, fmt.Errorf("the in-sandbox egress probe printed no verdict")
	}
	return rec, nil
}

// startAndWait streams a process to completion, returning its combined output
// and exit code. ExitEvent is the run's end — the signal a credential lease
// has to hang off, and the one sandboxd gives you for free.
func startAndWait(ctx context.Context, proc processv1.ProcessServiceClient, cfg *processv1.ProcessConfig) (string, int32, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	stream, err := proc.Start(ctx, &processv1.StartRequest{Config: cfg})
	if err != nil {
		return "", 0, fmt.Errorf("ProcessService.Start: %w", err)
	}

	var out strings.Builder
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out.String(), 0, fmt.Errorf("stream ended without an ExitEvent")
		}
		if err != nil {
			return out.String(), 0, fmt.Errorf("reading the process stream: %w", err)
		}
		switch {
		case ev.GetInit() != nil:
			fmt.Printf("process started in the sandbox (pid %d)\n", ev.GetInit().GetProcessId())
		case ev.GetStdout() != nil:
			out.Write(ev.GetStdout())
		case ev.GetStderr() != nil:
			out.Write(ev.GetStderr())
		case ev.GetExit() != nil:
			return out.String(), ev.GetExit().GetExitCode(), nil
		}
	}
}

// report asserts and prints the four properties. Every line carries the status
// code and body actually observed, so a reader can tell a real pass from a
// check that trivially could not have failed.
func report(o options, records []runRecord) error {
	if len(records) != 2 {
		return fmt.Errorf("expected 2 runs, got %d", len(records))
	}
	first, second := records[0], records[1]

	fmt.Printf("\n=== results ===\n")

	// 1. Accepted during the run. Read off the BODY, not the status: without a
	// real provider key the provider itself answers 401, so a status-only
	// assertion would confuse "gateway refused" with "provider refused". And
	// read acceptance off gatewayAccepted, not "didn't match a known refusal
	// string" — a gateway response this example has never seen must not read
	// as a pass. See gatewayAccepted's doc comment for why.
	if !gatewayAccepted(first.gateway) {
		if refusal := gatewayRefusal(first.gateway); refusal != "" {
			return fmt.Errorf("check 1: the gateway refused the run's own token (%q): %s",
				refusal, first.gateway.summary())
		}
		return fmt.Errorf("check 1: the gateway's response does not prove it proxied to the "+
			"provider (Content-Type %q, want application/json or text/event-stream): %s",
			first.gateway.ContentType, first.gateway.summary())
	}
	fmt.Printf("PASS 1/4  during the run, the gateway ACCEPTED the run's token and proxied the call\n")
	fmt.Printf("          gateway verdict:  ACCEPTED (the response below is the PROVIDER's, not the gateway's)\n")
	fmt.Printf("          in-sandbox call:  %s -> %s\n", o.gatewayInClusterHost, first.gateway.summary())
	fmt.Printf("          evidence:         Content-Type %q, which only a proxied provider response carries —\n",
		first.gateway.ContentType)
	fmt.Printf("                            every gateway-generated refusal is forced to text/plain by\n")
	fmt.Printf("                            net/http.Error, regardless of which refusal it is\n")

	// 2. Direct egress blocked, by IP, so DNS cannot be the explanation — and
	// only if the probe's own positive control succeeded, so a broken probe
	// cannot report a drop that never happened.
	if !strings.Contains(first.egress, "FAILED") {
		return fmt.Errorf("check 2: direct egress to %s was NOT blocked: %s", o.providerAddr, first.egress)
	}
	if !strings.Contains(first.egress, "control rc=0") {
		return fmt.Errorf("check 2: the probe's positive control did not succeed, so the "+
			"blocked verdict is not trustworthy: %s", first.egress)
	}
	fmt.Printf("PASS 2/4  from inside the sandbox, a DIRECT connection to the provider is blocked\n")
	fmt.Printf("          target:           %s (%s), by IP — never resolved inside the box\n",
		o.providerAddr, o.providerHost)
	fmt.Printf("          verdict:          %s\n", first.egress)
	fmt.Printf("          control:          the same probe reached the allowed gateway (control rc=0),\n")
	fmt.Printf("                            so this is the NetworkPolicy dropping packets — not DNS,\n")
	fmt.Printf("                            not a broken probe, not a dead network\n")

	// 3. The same token is dead after exit. Checked with the runner's own copy
	// of the token — i.e. against a credential that already left the sandbox,
	// which is the case that expiry-only schemes lose.
	res, err := callModelThroughGateway(context.Background(), o.gatewayURL, first.token.Token)
	if err != nil {
		return fmt.Errorf("check 3: %w", err)
	}
	if refusal := gatewayRefusal(res); refusal != "gateway token revoked" || res.Status != 401 {
		return fmt.Errorf("check 3: the token should be revoked after the run, got %s", res.summary())
	}
	fmt.Printf("PASS 3/4  after the process exited, that SAME token is refused\n")
	fmt.Printf("          replayed run %s's token (jti %s): %s\n", first.token.RunID, first.token.JTI, res.summary())
	fmt.Printf("          (the token has %s of validity left — it is revocation, not expiry, that killed it,\n",
		time.Until(first.token.ExpiresAt).Round(time.Second))
	fmt.Printf("           and the copy replayed here never went back into the sandbox)\n")

	// 4. Reuse is safe: a second run in the SAME box got its own working
	// credential while the first one's stayed dead.
	if first.token.RunID == second.token.RunID || first.token.JTI == second.token.JTI {
		return fmt.Errorf("check 4: the two runs shared an identity (run_id %s/%s, jti %s/%s)",
			first.token.RunID, second.token.RunID, first.token.JTI, second.token.JTI)
	}
	if !gatewayAccepted(second.gateway) {
		if refusal := gatewayRefusal(second.gateway); refusal != "" {
			return fmt.Errorf("check 4: the second run's own token was refused (%q): %s",
				refusal, second.gateway.summary())
		}
		return fmt.Errorf("check 4: the gateway's response to the second run's token does not "+
			"prove it proxied to the provider (Content-Type %q, want application/json or "+
			"text/event-stream): %s", second.gateway.ContentType, second.gateway.summary())
	}
	fmt.Printf("PASS 4/4  a second run in the SAME sandbox got a new run_id and a working token of its own\n")
	fmt.Printf("          run 1: run_id=%s jti=%s (revoked)\n", first.token.RunID, first.token.JTI)
	fmt.Printf("          run 2: run_id=%s jti=%s -> %s\n", second.token.RunID, second.token.JTI, second.gateway.summary())
	fmt.Printf("          (the box was never destroyed to expire a credential; see the README on why that\n")
	fmt.Printf("           matters for reuse)\n")

	fmt.Printf("\nAll 4 checks passed.\n")
	return nil
}

func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return "run-" + hex.EncodeToString(b), nil
}

func readTrimmed(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return []byte(s), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
