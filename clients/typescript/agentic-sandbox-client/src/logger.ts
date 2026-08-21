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

import type { Logger } from "./types.js";

/** Discards all log output. Used when SandboxClientOptions.quiet is set. */
export const noopLogger: Logger = {
  debug() {},
  info() {},
  warn() {},
  error() {},
};

function write(level: string, message: string): void {
  process.stderr.write(`[agentic-sandbox-client] ${level} ${message}\n`);
}

/** Writes all log levels to stderr, matching the Go client's default logger. */
export const defaultLogger: Logger = {
  debug: (message) => write("DEBUG", message),
  info: (message) => write("INFO", message),
  warn: (message) => write("WARN", message),
  error: (message) => write("ERROR", message),
};

/**
 * Resolves the effective logger from SandboxClientOptions: an explicit
 * `logger` takes precedence, otherwise `quiet` selects between the default
 * stderr logger and a no-op logger.
 */
export function resolveLogger(logger?: Logger, quiet?: boolean): Logger {
  if (logger) {
    return logger;
  }
  return quiet ? noopLogger : defaultLogger;
}
