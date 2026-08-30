# Scoped-token v2 deployment

The router reads its Ed25519 verification key set once at startup. Public verification keys belong in a ConfigMap. Private signing keys stay with the issuer and must never be mounted into the router.

Create the first versioned ConfigMap from a local key-set file, then apply this directory. Its `kustomization.yaml` uses `sandbox-router/deploy/` as its base and composes `deployment-patch.yaml` onto it before anything reaches the cluster, so no router Pod ever runs with the base Deployment's `allow-all` mode:

```sh
kubectl create configmap sandbox-router-scoped-token-keys-v1 --namespace agent-sandbox-system \
  --from-file=verification-keys.json=./verification-keys.json \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k sandbox-router/examples/scoped-token-v2/
kubectl rollout status deployment/sandbox-router --namespace agent-sandbox-system
```

Apply this overlay instead of `sandbox-router/deploy/`, not after it. Applying the core install first starts `allow-all` replicas that serve traffic until the patched rollout replaces them.

A downstream kustomization can take the same shape: reference `sandbox-router/deploy/` as its base and reuse `deployment-patch.yaml`.

The key-set file accepts overlapping readers. Each `publicKey` is the raw 32-byte Ed25519 public key in unpadded base64url, and each `kid` uses only ASCII letters, digits, hyphens, and underscores:

```json
{
  "keys": [
    {"kid": "previous", "publicKey": "<unpadded base64url Ed25519 public key>"},
    {"kid": "current", "publicKey": "<unpadded base64url Ed25519 public key>"}
  ]
}
```

Rotate keys reader first:

1. Create `sandbox-router-scoped-token-keys-v2` with both the previous and new public keys.
2. Change the ConfigMap name in `deployment-patch.yaml` to `v2`, then apply the overlay again. The Pod-template change rolls the Deployment because the router does not reload the file in place.
3. Wait for the rollout to finish before issuers mint tokens with the new `kid`.
4. Wait until every token signed by the previous key has expired.
5. Create the next versioned ConfigMap with only the current key, update the patch, and complete another rollout before deleting the old ConfigMap.

Keep the versioned ConfigMaps immutable in production. A chart or GitOps system may generate the names from content hashes, but that rollout policy belongs to the downstream deployment rather than this upstream example.
