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

use std::path::PathBuf;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    // The proto root lives next to this crate at the repo root.
    let proto_root: PathBuf = PathBuf::from("..").join("proto");
    let podagent_proto = proto_root.join("agentsandbox/podagent/v1/podagent.proto");
    let worker_proto = proto_root.join("agentsandbox/worker/v1/worker.proto");

    println!("cargo:rerun-if-changed={}", podagent_proto.display());
    println!("cargo:rerun-if-changed={}", worker_proto.display());

    // pod-agent crate hosts both the gRPC server (PodAgentService) and
    // the in-sandbox worker (WorkerService). We need both client+server
    // bindings: the agent is a server for PodAgent and a client for
    // Worker; the worker is the inverse.
    tonic_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_protos(&[podagent_proto, worker_proto], &[proto_root])?;
    Ok(())
}
