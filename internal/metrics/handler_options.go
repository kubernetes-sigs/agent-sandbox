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
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// BuildMetricsHandlerOpts returns promhttp handler options for the metrics server.
// The enableOpenMetrics parameter controls whether OpenMetrics format is enabled.
// This is important for forward compatibility with controller-runtime v0.26.0+,
// which enables OpenMetrics by default.
func BuildMetricsHandlerOpts(enableOpenMetrics bool) []func(*promhttp.HandlerOpts) {
	return []func(*promhttp.HandlerOpts){
		func(opts *promhttp.HandlerOpts) {
			opts.EnableOpenMetrics = enableOpenMetrics
		},
	}
}
