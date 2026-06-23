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
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// generateWebhookCerts generates a self-signed CA and a server certificate signed by that CA,
// or loads them from a shared Kubernetes Secret if it already exists.
// It writes the server certificate (tls.crt) and key (tls.key) to the certDir.
// It returns the PEM-encoded CA certificate, which is the caBundle to patch into the CRDs.
func generateWebhookCerts(ctx context.Context, c client.Client, certDir string, serviceName, namespace, clusterDomain string) ([]byte, error) {
	secretName := "agent-sandbox-webhook-certs"

	// 1. Try to get the existing shared secret
	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret)
	if err == nil {
		setupLog.Info("Found existing shared webhook certificates in Secret", "secret", secretName)
		// Extract certs from secret
		caPEM := secret.Data["ca.crt"]
		serverPEM := secret.Data["tls.crt"]
		serverKeyPEM := secret.Data["tls.key"]

		if len(caPEM) == 0 || len(serverPEM) == 0 || len(serverKeyPEM) == 0 {
			return nil, fmt.Errorf("shared Secret %s is missing certificate data", secretName)
		}

		// Write to local certDir for the webhook server
		if err := writeCertFiles(certDir, serverPEM, serverKeyPEM); err != nil {
			return nil, fmt.Errorf("failed to write certificate files locally: %w", err)
		}

		return caPEM, nil
	}

	if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to check for existing shared Secret: %w", err)
	}

	// 2. Secret does not exist; generate new certificates
	setupLog.Info("No shared webhook certificates found; generating new ones")
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA private key: %w", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "agent-sandbox-conversion-webhook-ca",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour), // 1 year validity
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	caBytes, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate server private key: %w", err)
	}

	dnsNames := []string{
		serviceName,
		fmt.Sprintf("%s.%s", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.%s", serviceName, namespace, clusterDomain),
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			CommonName: fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}

	serverBytes, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create server certificate: %w", err)
	}

	serverPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serverBytes,
	})

	serverKeyBytes, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal server private key: %w", err)
	}
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: serverKeyBytes,
	})

	// 3. Write to local certDir
	if err := writeCertFiles(certDir, serverPEM, serverKeyPEM); err != nil {
		return nil, fmt.Errorf("failed to write certificate files locally: %w", err)
	}

	// 4. Attempt to persist in a shared Secret
	sharedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":  caPEM,
			"tls.crt": serverPEM,
			"tls.key": serverKeyPEM,
		},
	}

	setupLog.Info("Creating shared webhook certificates Secret", "secret", secretName)
	err = c.Create(ctx, sharedSecret)
	if err == nil {
		return caPEM, nil
	}

	// 5. Handle race condition: if another replica created it concurrently
	if errors.IsAlreadyExists(err) {
		setupLog.Info("Shared Secret was created concurrently by another replica; loading it", "secret", secretName)
		secret = &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
			return nil, fmt.Errorf("failed to get concurrently created Secret: %w", err)
		}

		caPEM = secret.Data["ca.crt"]
		serverPEM = secret.Data["tls.crt"]
		serverKeyPEM = secret.Data["tls.key"]

		// Overwrite our local files with the other replica's certs
		if err := writeCertFiles(certDir, serverPEM, serverKeyPEM); err != nil {
			return nil, fmt.Errorf("failed to overwrite certificate files locally: %w", err)
		}

		return caPEM, nil
	}

	return nil, fmt.Errorf("failed to create shared Secret: %w", err)
}

// writeCertFiles writes the server certificate and key to the local certDir.
func writeCertFiles(certDir string, serverPEM, serverKeyPEM []byte) error {
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return err
	}

	certPath := filepath.Join(certDir, "tls.crt")
	keyPath := filepath.Join(certDir, "tls.key")

	if err := os.WriteFile(certPath, serverPEM, 0600); err != nil {
		return err
	}

	if err := os.WriteFile(keyPath, serverKeyPEM, 0600); err != nil {
		return err
	}

	return nil
}

// patchCRDs patches the CRDs in the cluster with the generated CA certificate and service details using a merge patch.
func patchCRDs(ctx context.Context, c client.Client, caPEM []byte, serviceName, namespace string) error {
	crdNames := []string{
		"sandboxes.agents.x-k8s.io",
		"sandboxclaims.extensions.agents.x-k8s.io",
		"sandboxtemplates.extensions.agents.x-k8s.io",
		"sandboxwarmpools.extensions.agents.x-k8s.io",
	}

	for _, name := range crdNames {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := c.Get(ctx, types.NamespacedName{Name: name}, crd)
		if err != nil {
			if errors.IsNotFound(err) {
				setupLog.Info("CRD not found, skipping patch", "crd", name)
				continue
			}
			return fmt.Errorf("failed to get CRD %s: %w", name, err)
		}

		if crd.Spec.Conversion == nil || crd.Spec.Conversion.Strategy != apiextensionsv1.WebhookConverter {
			setupLog.Info("CRD does not use Webhook conversion strategy, skipping patch", "crd", name)
			continue
		}

		// Keep a copy of the original CRD for the merge patch
		original := crd.DeepCopy()

		webhook := crd.Spec.Conversion.Webhook
		if webhook == nil {
			webhook = &apiextensionsv1.WebhookConversion{}
		}

		// Ensure ConversionReviewVersions is set when missing
		if len(webhook.ConversionReviewVersions) == 0 {
			webhook.ConversionReviewVersions = []string{"v1", "v1beta1"}
		}

		if webhook.ClientConfig == nil {
			webhook.ClientConfig = &apiextensionsv1.WebhookClientConfig{}
		}

		if webhook.ClientConfig.Service == nil {
			webhook.ClientConfig.Service = &apiextensionsv1.ServiceReference{}
		}

		// Update service details and caBundle
		webhook.ClientConfig.Service.Name = serviceName
		webhook.ClientConfig.Service.Namespace = namespace
		path := "/convert"
		webhook.ClientConfig.Service.Path = &path
		webhook.ClientConfig.CABundle = caPEM

		crd.Spec.Conversion.Webhook = webhook

		// Use Patch with MergeFrom to avoid write conflicts and managedFields issues
		if err := c.Patch(ctx, crd, client.MergeFrom(original)); err != nil {
			return fmt.Errorf("failed to patch CRD %s: %w", name, err)
		}

		setupLog.Info("Successfully patched CRD with webhook configuration", "crd", name)
	}

	return nil
}
