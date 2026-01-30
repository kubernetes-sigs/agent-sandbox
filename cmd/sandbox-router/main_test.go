package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthCheck(t *testing.T) {
	req, err := http.NewRequest("GET", "/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(healthCheck)

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	expected := `{"status":"ok"}`
	if strings.TrimSpace(rr.Body.String()) != expected {
		t.Errorf("handler returned unexpected body: got %v want %v",
			rr.Body.String(), expected)
	}
}

func TestProxyRequest_Validation(t *testing.T) {
	tests := []struct {
		name           string
		headers        map[string]string
		expectedStatus int
		expectedBody   string
	}{
		{
			name:           "Missing Sandbox ID",
			headers:        map[string]string{},
			expectedStatus: http.StatusBadRequest,
			expectedBody:   "X-Sandbox-ID header is required.",
		},
		{
			name: "Invalid Namespace",
			headers: map[string]string{
				"X-Sandbox-ID":        "test",
				"X-Sandbox-Namespace": "invalid/namespace",
			},
			expectedStatus: http.StatusBadRequest,
			expectedBody:   "Invalid namespace format.",
		},
		{
			name: "Invalid Port",
			headers: map[string]string{
				"X-Sandbox-ID":   "test",
				"X-Sandbox-Port": "invalid-port",
			},
			expectedStatus: http.StatusBadRequest,
			expectedBody:   "Invalid port format.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", "/", nil)
			if err != nil {
				t.Fatal(err)
			}

			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			rr := httptest.NewRecorder()
			handler := http.HandlerFunc(proxyRequest)

			handler.ServeHTTP(rr, req)

			if status := rr.Code; status != tt.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v",
					status, tt.expectedStatus)
			}

			if !strings.Contains(strings.TrimSpace(rr.Body.String()), tt.expectedBody) {
				t.Errorf("handler returned unexpected body: got %v want containing %v",
					rr.Body.String(), tt.expectedBody)
			}
		})
	}
}

// TestProxyRequest_Success is harder to test with httptest because the proxy
// tries to connect to a specific hostname (cluster.local) which doesn't exist.
// However, we can test that it attempts to connect or that the Director logic works.
// Given the simplicity, validation tests give good confidence.
// A full integration test would require mocking the network or DNS, which is overkill here.
