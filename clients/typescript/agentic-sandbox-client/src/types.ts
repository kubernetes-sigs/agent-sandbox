// Copyright 2025 The Kubernetes Authors.
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

export interface SandboxClientOptions {
  namespace?: string;
  apiUrl?: string;
  gatewayName?: string;
  gatewayNamespace?: string;
  serverPort?: number;
  sandboxReadyTimeout?: number;
  gatewayReadyTimeout?: number;
  portForwardReadyTimeout?: number;
  enableTracing?: boolean;
  traceServiceName?: string;
}

export interface CreateSandboxOptions {
  sandboxReadyTimeout?: number;
  labels?: Record<string, string>;
}

export interface ExecutionResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

export interface FileEntry {
  name: string;
  size: number;
  type: "file" | "directory";
  modTime: number;
}

export type RequestFn = (
  method: string,
  endpoint: string,
  options?: {
    body?: BodyInit | null;
    headers?: Record<string, string>;
    timeout?: number;
    maxRetries?: number;
  },
) => Promise<Response>;
