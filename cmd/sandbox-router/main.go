package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

const (
	defaultSandboxPort = "8888"
	defaultNamespace   = "default"
	listenPort         = ":8080"
)

var (
	namespaceRegex = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", healthCheck)
	mux.HandleFunc("/", proxyRequest)

	server := &http.Server{
		Addr:         listenPort,
		Handler:      mux,
		ReadTimeout:  180 * time.Second, // Incoming request timeout
		WriteTimeout: 180 * time.Second,
	}

	log.Printf("Starting sandbox router on %s", listenPort)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func proxyRequest(w http.ResponseWriter, r *http.Request) {
	// 1. Get Sandbox ID
	sandboxID := r.Header.Get("X-Sandbox-ID")
	if sandboxID == "" {
		http.Error(w, "X-Sandbox-ID header is required.", http.StatusBadRequest)
		return
	}

	// 2. Get Namespace
	namespace := r.Header.Get("X-Sandbox-Namespace")
	if namespace == "" {
		namespace = defaultNamespace
	}

	// Sanitize namespace
	if !namespaceRegex.MatchString(namespace) {
		http.Error(w, "Invalid namespace format.", http.StatusBadRequest)
		return
	}

	// 3. Get Port
	portStr := r.Header.Get("X-Sandbox-Port")
	if portStr == "" {
		portStr = defaultSandboxPort
	}
	if _, err := strconv.Atoi(portStr); err != nil {
		http.Error(w, "Invalid port format.", http.StatusBadRequest)
		return
	}

	// 4. Construct Target URL
	// {sandbox_id}.{namespace}.svc.cluster.local
	targetHost := fmt.Sprintf("%s.%s.svc.cluster.local:%s", sandboxID, namespace, portStr)
	
	// Construct the target URL schema and host
	targetURL := &url.URL{
		Scheme: "http",
		Host:   targetHost,
	}

	log.Printf("Proxying request for sandbox '%s' to URL: http://%s%s", sandboxID, targetHost, r.URL.Path)

	// 5. Create Reverse Proxy
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.Host = targetURL.Host // Set the Host header to the target
		},
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 180 * time.Second, // Match Python httpx timeout
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			log.Printf("ERROR: Connection to sandbox at %s failed. Error: %v", targetHost, err)
			http.Error(w, fmt.Sprintf("Could not connect to the backend sandbox: %s", sandboxID), http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}
