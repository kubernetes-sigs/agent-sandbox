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

//! Pod-agent → worker gRPC client over Unix domain sockets.

use std::path::PathBuf;

use anyhow::Context;
use hyper_util::rt::TokioIo;
use tokio::net::UnixStream;
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;

pub mod worker_proto {
    tonic::include_proto!("agentsandbox.worker.v1");
}

/// Open a gRPC channel to the worker listening on the given Unix socket.
///
/// The URI scheme is irrelevant — tonic insists on a URI but we plug a
/// custom connector that ignores it and dials the Unix socket directly.
pub async fn connect(socket_path: PathBuf) -> anyhow::Result<Channel> {
    let endpoint = Endpoint::try_from("http://localhost").context("endpoint")?;
    let channel = endpoint
        .connect_with_connector(service_fn(move |_: Uri| {
            let path = socket_path.clone();
            async move {
                let stream = UnixStream::connect(path).await?;
                Ok::<_, std::io::Error>(TokioIo::new(stream))
            }
        }))
        .await
        .context("dial worker uds")?;
    Ok(channel)
}
