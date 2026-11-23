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

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	sandboxclient "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/typed/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/sandbox-router/authz"
)

func mintRouterToken(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("mint-router-token", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: agent-sandbox-controller mint-router-token [flags] <sandbox-name>")
		flags.PrintDefaults()
	}
	keyFile := flags.String("signing-key-file", "", "PEM file containing a PKCS#8 Ed25519 private key (required)")
	keyID := flags.String("key-id", "", "Signing key ID in the router's verification key set (required)")
	ttl := flags.Duration("ttl", 5*time.Minute, "Token lifetime (at least 1s)")
	namespace := flags.String("namespace", "", "Sandbox namespace (defaults to the kubeconfig context namespace)")
	kubeconfig := flags.String("kubeconfig", "", "Path to the kubeconfig file")
	kubeContext := flags.String("context", "", "Name of the kubeconfig context to use")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || *keyFile == "" || *keyID == "" {
		flags.Usage()
		return fmt.Errorf("a Sandbox name, --signing-key-file, and --key-id are required")
	}
	if *ttl < time.Second {
		return fmt.Errorf("--ttl must be at least 1s")
	}
	key, err := readRouterSigningKey(*keyFile)
	if err != nil {
		return err
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = *kubeconfig
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{
		CurrentContext: *kubeContext,
		Context:        clientcmdapi.Context{Namespace: *namespace},
	})
	restConfig, err := config.ClientConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes client config: %w", err)
	}
	ns, _, err := config.Namespace()
	if err != nil {
		return fmt.Errorf("resolve Sandbox namespace: %w", err)
	}
	client, err := sandboxclient.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Sandbox client: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sandbox, err := client.Sandboxes(ns).Get(ctx, flags.Arg(0), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get Sandbox %s/%s: %w", ns, flags.Arg(0), err)
	}
	if !sandbox.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Sandbox %s/%s is being deleted", sandbox.Namespace, sandbox.Name)
	}
	token, err := authz.MintScopedTokenV2Sandbox(key, *keyID, sandbox.Namespace, sandbox.Name, string(sandbox.UID), *ttl)
	if err != nil {
		return fmt.Errorf("mint router token: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, token); err != nil {
		return fmt.Errorf("write router token: %w", err)
	}
	return nil
}

func readRouterSigningKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	block, rest := pem.Decode(bytes.TrimSpace(data))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("signing key must be a single PKCS#8 private key in PEM format")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key: %w", err)
	}
	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing key must be an Ed25519 private key")
	}
	return privateKey, nil
}
