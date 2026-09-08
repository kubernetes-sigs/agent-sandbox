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
  sandboxReadyTimeout?: number;
  enableTracing?: boolean;
  traceServiceName?: string;
  /**
   * Logger used for diagnostic output (lifecycle events, retries, warnings).
   * Defaults to a logger that writes to stderr. Mirrors the Go client's
   * Options.Logger.
   */
  logger?: Logger;
  /**
   * Suppresses the default stderr logger. Ignored when `logger` is set.
   * Mirrors the Go client's Options.Quiet.
   */
  quiet?: boolean;
}

/**
 * Diagnostic logger injectable via SandboxClientOptions.logger.
 * Mirrors the Go client's logr.Logger usage (Options.Logger).
 */
export interface Logger {
  debug(message: string): void;
  info(message: string): void;
  warn(message: string): void;
  error(message: string): void;
}

export interface CreateSandboxOptions {
  sandboxReadyTimeout?: number;
  labels?: Record<string, string>;
}
