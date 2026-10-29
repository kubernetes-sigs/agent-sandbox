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

package server

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// ExecutionPolicy supplies defaults and ceilings for both unary execution RPCs.
// Zero values leave a setting unconfigured. Start and PTY are unaffected.
type ExecutionPolicy struct {
	DefaultTimeLimit        time.Duration
	MaxTimeLimit            time.Duration
	DefaultStdoutLimitBytes uint64
	MaxStdoutLimitBytes     uint64
	DefaultStderrLimitBytes uint64
	MaxStderrLimitBytes     uint64
}

func (p ExecutionPolicy) validate() error {
	if p.DefaultTimeLimit < 0 || p.MaxTimeLimit < 0 {
		return errors.New("execution time defaults and maxima must not be negative")
	}
	if p.MaxTimeLimit > 0 && p.DefaultTimeLimit > p.MaxTimeLimit {
		return errors.New("default execution time limit exceeds its maximum")
	}
	for _, stream := range []struct {
		name     string
		def, max uint64
	}{
		{"stdout", p.DefaultStdoutLimitBytes, p.MaxStdoutLimitBytes},
		{"stderr", p.DefaultStderrLimitBytes, p.MaxStderrLimitBytes},
	} {
		if stream.max > 0 && stream.def > stream.max {
			return fmt.Errorf("default %s byte limit exceeds its maximum", stream.name)
		}
	}
	return nil
}

func (p ExecutionPolicy) resolve(req *processv1.ExecuteWithLimitsRequest) (executionLimits, error) {
	if req == nil {
		req = &processv1.ExecuteWithLimitsRequest{}
	}
	timeLimit, err := requestedTimeLimit(req.GetTimeLimit())
	if err != nil {
		return executionLimits{}, err
	}
	if p.MaxTimeLimit > 0 && timeLimit > p.MaxTimeLimit {
		return executionLimits{}, status.Error(codes.InvalidArgument, "time_limit exceeds deployment maximum")
	}
	if timeLimit == 0 {
		timeLimit = p.DefaultTimeLimit
	}
	if timeLimit == 0 {
		timeLimit = p.MaxTimeLimit
	}
	stdout, err := resolveByteLimit("stdout_limit_bytes", req.StdoutLimitBytes, p.DefaultStdoutLimitBytes, p.MaxStdoutLimitBytes)
	if err != nil {
		return executionLimits{}, err
	}
	stderr, err := resolveByteLimit("stderr_limit_bytes", req.StderrLimitBytes, p.DefaultStderrLimitBytes, p.MaxStderrLimitBytes)
	if err != nil {
		return executionLimits{}, err
	}
	return executionLimits{timeLimit: timeLimit, stdoutBytes: stdout, stderrBytes: stderr}, nil
}

func resolveByteLimit(name string, requested *uint64, def, max uint64) (uint64, error) {
	if requested != nil {
		if *requested == 0 {
			return 0, status.Errorf(codes.InvalidArgument, "%s must be positive", name)
		}
		if max > 0 && *requested > max {
			return 0, status.Errorf(codes.InvalidArgument, "%s exceeds deployment maximum", name)
		}
		return *requested, nil
	}
	if def != 0 {
		return def, nil
	}
	return max, nil
}
