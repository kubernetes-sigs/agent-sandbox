# KEP-NNNN: Go-Based Sandbox Router with Pod IP Mapping

<!--
TOC is auto-generated via `make toc-update`.
-->

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
- [Proposal](#proposal)
  - [High-Level Design](#high-level-design)
    - [API Changes](#api-changes)
    - [Implementation Guidance](#implementation-guidance)
- [Scalability](#scalability)
- [Alternatives (Optional)](#alternatives-optional)
<!-- /toc -->

## Summary

This KEP proposes adding a dedicated **Sandbox Router** as a top-level component in Go. It will replace the current Python-based prototype. The Go router will utilize an in-memory mapping of Sandbox IDs to Pod IPs (populated by watching K8s events) to achieve extremely fast route setup and low-latency proxying, while incorporating robust mechanisms to handle state drift.

## Motivation

The current sandbox router is a prototype implemented in Python using FastAPI. While functional, it has several limitations:
1. **Performance Overhead**: Python is not ideal for a high-throughput, low-latency reverse proxy component.
2. **Route Setup Latency**: It relies on Kubernetes Services and DNS resolution (`{sandbox-id}.{namespace}.svc.cluster.local`). Creating Services and waiting for DNS propagation introduces latency that slows down the rapid "just-in-time" sandbox creation needed for AI agents.
3. **Security**: The current prototype trusts headers blindly and lacks advanced routing controls or integration with the broader system's security model.

We need a component that is:
* **Fast**: Instantaneous route availability.
* **Lightweight**: Low resource consumption.
* **Secure**: Capable of enforcing access controls.
* **Resilient**: Able to handle rapid pod churn and API server disconnects.

## Proposal

We propose building a Go-based Sandbox Router. 

### High-Level Design

The router will act as a Layer 7 reverse proxy. Instead of relying on K8s DNS for every request, it will maintain an internal, high-speed map of active sandboxes.

#### Core Mechanism: Memory-Mapped Pod Routing
1. **K8s Informer**: The router will run a Kubernetes Informer watching Pods in the sandbox namespace with specific labels (e.g., `sandbox-id`).
2. **In-Memory Table**: On receiving `ADD` or `UPDATE` events for Pods that are `Ready`, the router will add/update a mapping of **Sandbox CRD UID** (read from Pod labels) to `PodIP` in a thread-safe map.
3. **Direct Proxying**: When a request arrives, the router expects both `X-Sandbox-ID` (for logging/fallback) and `X-Sandbox-UID` (for secure routing). It looks up the IP using the UID in its map and proxies the traffic directly to the Pod IP, bypassing K8s Service overhead.

#### Addressing State Drift
Maintaining a local cache of cluster state introduces the risk of **drift** (stale data). We will implement the following mitigations:
* **Active Cache Invalidation**: If the router attempts to connect to a cached Pod IP and receives a connection error (suggesting the Pod died before the delete event was processed), it will immediately invalidate that cache entry.
* **Hybrid Fallback (DNS)**: If a Sandbox ID is not found in the map, or if the mapped IP fails, the router will fall back to attempting resolution via standard K8s DNS (`{sandbox-id}.{namespace}.svc.cluster.local`). This ensures that even if the Informer is lagging or partitioned, requests can still succeed via the standard K8s networking path.
* **Sync Period**: A short resync period for the informer to ensure any missed events are reconciled quickly.

### API Changes

We introduce a new header for secure routing, while maintaining backward compatibility where possible:
* `X-Sandbox-ID` (Required: Used for human reference, logging, and DNS fallback).
* `X-Sandbox-UID` (Required for secure routing: The UID of the Sandbox custom resource).
* `X-Sandbox-Namespace` (Optional, defaults to configured default).
* `X-Sandbox-Port` (Optional, defaults to 8888).

### Security Considerations

Rewriting the router in Go provides several security advantages over the current Python prototype:

1. **Strict Input Validation and Sanitization**: Go's performance allows for rigorous validation of headers (like `X-Sandbox-ID`) without performance degradation, preventing potential DNS injection or path traversal attacks.
2. **Authentication and Authorization Integration**: The Go router can be easily integrated with the Kubernetes `TokenReview` API or custom authentication mechanisms. This allows the router to verify that the requesting client has permission to access the specific sandbox, moving away from the "blind trust" model of the requested prototype.
3. **Isolation of Target Resolution**: By using an internal map of `UID` to `PodIP`, the router prevents clients from manipulating requests to route to arbitrary internal services. The target is strictly bound to a valid Sandbox Pod IP discovered by the Informer, and accessed via a non-guessable UID.
4. **Denial of Service (DoS) Resilience**: Go's concurrency model (goroutines) handles high numbers of concurrent requests much better than Python's async loop in edge cases, making the router more resilient to resource exhaustion and slow-client attacks.

### Implementation Guidance

* **Language**: Go.
* **Libraries**: Use standard library `net/http/httputil.ReverseProxy` for the proxy implementation. Use `client-go` for the Informer and Authentication APIs.

#### Authentication Integration (TokenReview)

To implement authentication, the router will use a middleware that extracts a Bearer token from the `Authorization` header and validates it against the Kubernetes API using the `TokenReview` resource.

```go
func AuthMiddleware(clientset kubernetes.Interface, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")

		tokenReview := &authenticationv1.TokenReview{
			Spec: authenticationv1.TokenReviewSpec{Token: token},
		}

		result, err := clientset.AuthenticationV1().TokenReviews().Create(
			context.Background(), tokenReview, metav1.CreateOptions{})
		if err != nil || !result.Status.Authenticated {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// TODO: Add Authorization check (e.g. SubjectAccessReview)
		next.ServeHTTP(w, r)
	})
}
```

**Notes**:
* **Permissions**: The router needs a ServiceAccount with a ClusterRole permitting `create` on `tokenreviews`.
* **Caching**: Successful token reviews must be cached with a short TTL to maintain proxy performance.


## Scalability

* **Memory**: An in-memory map of strings to strings for thousands of sandboxes will consume minimal memory (a few megabytes).
* **CPU**: Go's `httputil.ReverseProxy` is highly optimized and can handle thousands of concurrent requests with low CPU usage.
* **Network**: Direct routing to Pod IPs reduces the number of hops and avoids kube-proxy (iptables/IPVS) overhead.

## Alternatives (Optional)

1. **Header-Only Dynamic Routing (Stateless)**: We could rewrite the current Python logic in Go without the in-memory map. This avoids drift issues entirely but retains the dependency on K8s DNS propagation time for new sandboxes.
2. **Envoy / Traefik**: We could use an off-the-shelf proxy like Envoy. However, configuring Envoy dynamically for thousands of ad-hoc routes requires a complex control plane (xDS). A custom Go microservice is simpler and lighter for our specific use case.
3. **Rama**: A Rust-based web framework with an async reverse proxy. While fast, it requires Rust runtime which is not a standard runtime in k8s project.
4. **K8s Service**: We could use the current K8s Service for routing. While simple, it introduces latency due to K8s DNS propagation time for new sandboxes.
