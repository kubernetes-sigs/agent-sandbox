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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGenerateWebhookCerts(t *testing.T) {
	scheme := runtime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)

	serviceName := "test-service"
	namespace := "test-namespace"
	clusterDomain := "cluster.local"
	secretName := "agent-sandbox-webhook-certs"

	t.Run("successfully generates new certs when Secret does not exist", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "webhook-certs-test-*")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

		caPEM, err := generateWebhookCerts(context.Background(), fakeClient, tempDir, serviceName, namespace, clusterDomain)
		require.NoError(t, err)
		require.NotEmpty(t, caPEM)

		// 1. Verify files are written locally
		certPath := filepath.Join(tempDir, "tls.crt")
		keyPath := filepath.Join(tempDir, "tls.key")
		assert.FileExists(t, certPath)
		assert.FileExists(t, keyPath)

		// 2. Verify server certificate has correct DNS SANs
		certBytes, err := os.ReadFile(certPath)
		require.NoError(t, err)
		certBlock, _ := pem.Decode(certBytes)
		require.NotNil(t, certBlock)
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		require.NoError(t, err)

		expectedDNSNames := []string{
			"test-service",
			"test-service.test-namespace",
			"test-service.test-namespace.svc",
			"test-service.test-namespace.svc.cluster.local",
		}
		assert.ElementsMatch(t, expectedDNSNames, cert.DNSNames)

		// 3. Verify the Secret was created in the cluster
		secret := &corev1.Secret{}
		err = fakeClient.Get(context.Background(), types.NamespacedName{Name: secretName, Namespace: namespace}, secret)
		require.NoError(t, err)
		assert.Equal(t, caPEM, secret.Data["ca.crt"])
		assert.NotEmpty(t, secret.Data["tls.crt"])
		assert.NotEmpty(t, secret.Data["tls.key"])
	})

	t.Run("successfully loads existing certs from Secret when it exists", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "webhook-certs-test-*")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		// Pre-populate Secret with dummy data
		existingCA := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----")
		existingCert := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----")
		existingKey := []byte("-----BEGIN EC PRIVATE KEY-----\nMIIB\n-----END EC PRIVATE KEY-----")

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: namespace,
			},
			Data: map[string][]byte{
				"ca.crt":  existingCA,
				"tls.crt": existingCert,
				"tls.key": existingKey,
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

		caPEM, err := generateWebhookCerts(context.Background(), fakeClient, tempDir, serviceName, namespace, clusterDomain)
		require.NoError(t, err)
		assert.Equal(t, existingCA, caPEM)

		// Verify files are written locally with the pre-populated values
		certPath := filepath.Join(tempDir, "tls.crt")
		keyPath := filepath.Join(tempDir, "tls.key")

		certBytes, err := os.ReadFile(certPath)
		require.NoError(t, err)
		assert.Equal(t, existingCert, certBytes)

		keyBytes, err := os.ReadFile(keyPath)
		require.NoError(t, err)
		assert.Equal(t, existingKey, keyBytes)
	})

	t.Run("returns error when existing Secret has invalid certificate data", func(t *testing.T) {
		tempDir, err := os.MkdirTemp("", "webhook-certs-test-*")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir)

		// Pre-populate Secret with invalid PEM data
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: namespace,
			},
			Data: map[string][]byte{
				"ca.crt":  []byte("invalid-ca-pem"),
				"tls.crt": []byte("invalid-cert-pem"),
				"tls.key": []byte("invalid-key-pem"),
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

		caPEM, err := generateWebhookCerts(context.Background(), fakeClient, tempDir, serviceName, namespace, clusterDomain)
		require.Error(t, err)
		assert.Nil(t, caPEM)
		assert.Contains(t, err.Error(), "has invalid certificate data")
	})
}

func TestPatchCRDs(t *testing.T) {
	scheme := runtime.NewScheme()
	err := apiextensionsv1.AddToScheme(scheme)
	require.NoError(t, err)

	// Create a helper function to build a fake CRD
	makeCRD := func(name string, hasWebhook bool) *apiextensionsv1.CustomResourceDefinition {
		crd := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "agents.x-k8s.io",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Kind: "Sandbox",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{
						Name:    "v1beta1",
						Served:  true,
						Storage: true,
					},
					{
						Name:    "v1alpha1",
						Served:  true,
						Storage: false,
					},
				},
			},
		}
		if hasWebhook {
			crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ConversionReviewVersions: []string{"v1", "v1beta1"},
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						Service: &apiextensionsv1.ServiceReference{
							Name:      "old-service",
							Namespace: "old-namespace",
						},
						CABundle: []byte("old-ca"),
					},
				},
			}
		} else {
			crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.NoneConverter,
			}
		}
		return crd
	}

	t.Run("successfully patches CRDs with Webhook strategy", func(t *testing.T) {
		crd1 := makeCRD("sandboxes.agents.x-k8s.io", true)
		crd2 := makeCRD("sandboxclaims.extensions.agents.x-k8s.io", true)
		// crd3 is not installed (simulating extensions disabled)
		crd4 := makeCRD("sandboxwarmpools.extensions.agents.x-k8s.io", false) // has None strategy

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(crd1, crd2, crd4).
			Build()

		caPEM := []byte("new-ca-pem")
		serviceName := "new-service"
		namespace := "new-namespace"

		err := patchCRDs(context.Background(), fakeClient, caPEM, serviceName, namespace)
		require.NoError(t, err)

		// Verify crd1 was patched
		patchedCRD1 := &apiextensionsv1.CustomResourceDefinition{}
		err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "sandboxes.agents.x-k8s.io"}, patchedCRD1)
		require.NoError(t, err)
		require.NotNil(t, patchedCRD1.Spec.Conversion)
		require.NotNil(t, patchedCRD1.Spec.Conversion.Webhook)
		assert.Equal(t, serviceName, patchedCRD1.Spec.Conversion.Webhook.ClientConfig.Service.Name)
		assert.Equal(t, namespace, patchedCRD1.Spec.Conversion.Webhook.ClientConfig.Service.Namespace)
		assert.Equal(t, "/convert", *patchedCRD1.Spec.Conversion.Webhook.ClientConfig.Service.Path)
		assert.Equal(t, caPEM, patchedCRD1.Spec.Conversion.Webhook.ClientConfig.CABundle)

		// Verify crd2 was patched
		patchedCRD2 := &apiextensionsv1.CustomResourceDefinition{}
		err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "sandboxclaims.extensions.agents.x-k8s.io"}, patchedCRD2)
		require.NoError(t, err)
		assert.Equal(t, serviceName, patchedCRD2.Spec.Conversion.Webhook.ClientConfig.Service.Name)
		assert.Equal(t, caPEM, patchedCRD2.Spec.Conversion.Webhook.ClientConfig.CABundle)

		// Verify crd4 was NOT patched (strategy remains None)
		patchedCRD4 := &apiextensionsv1.CustomResourceDefinition{}
		err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "sandboxwarmpools.extensions.agents.x-k8s.io"}, patchedCRD4)
		require.NoError(t, err)
		assert.Equal(t, apiextensionsv1.NoneConverter, patchedCRD4.Spec.Conversion.Strategy)
		assert.Nil(t, patchedCRD4.Spec.Conversion.Webhook)
	})
}

func TestResolveWebhookCertFiles(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		dirs     []string
		certName string
		keyName  string
		wantCert string
		wantKey  string
		wantErr  bool
	}{
		{name: "default pair present", files: []string{"tls.crt", "tls.key"}, certName: "tls.crt", keyName: "tls.key", wantCert: "tls.crt", wantKey: "tls.key"},
		{name: "custom filenames present", files: []string{"server.crt", "server.key"}, certName: "server.crt", keyName: "server.key", wantCert: "server.crt", wantKey: "server.key"},
		{name: "combined file requested explicitly for both", files: []string{"cert.pem"}, certName: "cert.pem", keyName: "cert.pem", wantCert: "cert.pem", wantKey: "cert.pem"},
		{name: "fallback to combined file when defaults absent", files: []string{"cert.pem"}, certName: "tls.crt", keyName: "tls.key", wantCert: "cert.pem", wantKey: "cert.pem"},
		{name: "no fallback for custom names", files: []string{"cert.pem"}, certName: "server.crt", keyName: "server.key", wantErr: true},
		{name: "no fallback when only key is missing", files: []string{"tls.crt", "cert.pem"}, certName: "tls.crt", keyName: "tls.key", wantErr: true},
		{name: "no fallback when only cert is missing", files: []string{"tls.key", "cert.pem"}, certName: "tls.crt", keyName: "tls.key", wantErr: true},
		{name: "all files missing", certName: "tls.crt", keyName: "tls.key", wantErr: true},
		{name: "cert path is a directory", dirs: []string{"tls.crt"}, files: []string{"tls.key"}, certName: "tls.crt", keyName: "tls.key", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("pem"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, d := range tc.dirs {
				if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cert, key, err := resolveWebhookCertFiles(dir, tc.certName, tc.keyName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got cert=%q key=%q", cert, key)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cert != tc.wantCert || key != tc.wantKey {
				t.Fatalf("got (%q, %q), want (%q, %q)", cert, key, tc.wantCert, tc.wantKey)
			}
		})
	}

	t.Run("validates that resolved combined cert.pem parses with tls.LoadX509KeyPair", func(t *testing.T) {
		dir := t.TempDir()

		// Generate valid EC key pair and self-signed certificate
		privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)

		template := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: "localhost"},
			NotBefore:    time.Now().Add(-1 * time.Hour),
			NotAfter:     time.Now().Add(1 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}

		certBytes, err := x509.CreateCertificate(rand.Reader, template, template, &privKey.PublicKey, privKey)
		require.NoError(t, err)

		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certBytes})
		keyBytes, err := x509.MarshalECPrivateKey(privKey)
		require.NoError(t, err)
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

		// Concatenate certificate and key into single cert.pem
		combinedPEM := append(certPEM, keyPEM...)
		certPemPath := filepath.Join(dir, "cert.pem")
		require.NoError(t, os.WriteFile(certPemPath, combinedPEM, 0o600))

		// Resolve cert files (expecting fallback to cert.pem)
		resolvedCert, resolvedKey, err := resolveWebhookCertFiles(dir, "tls.crt", "tls.key")
		require.NoError(t, err)
		assert.Equal(t, "cert.pem", resolvedCert)
		assert.Equal(t, "cert.pem", resolvedKey)

		// Verify tls.LoadX509KeyPair successfully parses the single combined file
		tlsKeyPair, err := tls.LoadX509KeyPair(filepath.Join(dir, resolvedCert), filepath.Join(dir, resolvedKey))
		require.NoError(t, err)
		assert.NotEmpty(t, tlsKeyPair.Certificate)
	})
}
