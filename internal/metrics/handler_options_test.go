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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// TestBuildMetricsHandlerOpts verifies that BuildMetricsHandlerOpts returns
// handler options that correctly set EnableOpenMetrics based on the parameter.
func TestBuildMetricsHandlerOpts(t *testing.T) {
	tests := []struct {
		name              string
		enableOpenMetrics bool
		wantEnabled       bool
	}{
		{"enabled", true, true},
		{"disabled", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := BuildMetricsHandlerOpts(tt.enableOpenMetrics)
			if len(opts) == 0 {
				t.Fatal("expected non-empty handler options")
			}

			// Apply all option functions to a HandlerOpts and verify EnableOpenMetrics
			handlerOpts := &promhttp.HandlerOpts{}
			for _, fn := range opts {
				fn(handlerOpts)
			}

			if handlerOpts.EnableOpenMetrics != tt.wantEnabled {
				t.Errorf("EnableOpenMetrics = %v, want %v", handlerOpts.EnableOpenMetrics, tt.wantEnabled)
			}
		})
	}
}

// TestBuildMetricsHandlerOptsMultipleApplications verifies that applying the
// handler options multiple times is idempotent and doesn't cause issues.
func TestBuildMetricsHandlerOptsMultipleApplications(t *testing.T) {
	opts := BuildMetricsHandlerOpts(true)
	handlerOpts := &promhttp.HandlerOpts{}

	// Apply options multiple times
	for range 3 {
		for _, fn := range opts {
			fn(handlerOpts)
		}
	}

	if !handlerOpts.EnableOpenMetrics {
		t.Error("EnableOpenMetrics should remain true after multiple applications")
	}
}
