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

package managedsandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

// SSH port that pod-agents listen on; baked into per-pool-pod Service.
const sshPort int32 = 2222

// secretKeyToken is the Secret data key holding the SSH session token.
const secretKeyToken = "token"

// ensureSSHToken creates (or fetches existing) the per-Sandbox Secret
// holding the SSH session token, and returns its decoded value. The
// Secret is owned by the Sandbox so it is GC'd on delete.
//
// Token rotation is not supported yet: once minted the token sticks for
// the life of the Sandbox. To rotate, delete the Secret manually.
func (r *ManagedSandboxReconciler) ensureSSHToken(ctx context.Context, sandbox *sandboxv1alpha1.ManagedSandbox) (string, string, error) {
	name := sandbox.Name + "-ssh"
	nsn := types.NamespacedName{Namespace: sandbox.Namespace, Name: name}

	existing := &corev1.Secret{}
	err := r.Get(ctx, nsn, existing)
	if err == nil {
		token, ok := existing.Data[secretKeyToken]
		if !ok || len(token) == 0 {
			return "", "", fmt.Errorf("ssh secret %s exists but has no %q key", name, secretKeyToken)
		}
		return string(token), name, nil
	}
	if !k8serrors.IsNotFound(err) {
		return "", "", fmt.Errorf("get ssh secret: %w", err)
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", "", fmt.Errorf("generate ssh token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				sandboxLabel: NameHash(sandbox.Name),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			secretKeyToken: []byte(token),
		},
	}
	if err := ctrl.SetControllerReference(sandbox, secret, r.Scheme, ctrlutil.WithBlockOwnerDeletion(false)); err != nil {
		return "", "", fmt.Errorf("set owner on ssh secret: %w", err)
	}
	if err := r.Create(ctx, secret, client.FieldOwner(managedSandboxControllerFieldOwner)); err != nil {
		// Race: another reconcile created it between our Get and Create.
		if k8serrors.IsAlreadyExists(err) {
			return r.ensureSSHToken(ctx, sandbox)
		}
		return "", "", fmt.Errorf("create ssh secret: %w", err)
	}
	return token, name, nil
}
