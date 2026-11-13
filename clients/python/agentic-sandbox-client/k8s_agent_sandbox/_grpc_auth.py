# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Channel-level sandboxd metadata, imported only when gRPC is used.

Interceptors add credentials and routing metadata without consuming streams or changing RPC
lifetimes, deadlines, cancellation, or error handling.
"""

from collections.abc import Callable, Iterable, Mapping
from typing import Any

import grpc


def _metadata(
    original: Iterable[tuple[str, str | bytes]] | None, configured: Mapping[str, str]
) -> tuple[tuple[str, str | bytes], ...]:
    # Preserve duplicate and binary application metadata. SDK credentials and
    # routing metadata take precedence over caller-supplied values.
    return tuple(
        (key, value) for key, value in (original or ())
        if key.lower() not in configured
    ) + tuple(configured.items())


class _ClientCallDetails(grpc.ClientCallDetails):
    def __init__(self, original: Any, metadata: Mapping[str, str]) -> None:
        self.method = original.method
        self.timeout = original.timeout
        self.metadata = _metadata(original.metadata, metadata)
        self.credentials = original.credentials
        self.wait_for_ready = getattr(original, "wait_for_ready", None)
        self.compression = getattr(original, "compression", None)


class MetadataInterceptor(
    grpc.UnaryUnaryClientInterceptor,
    grpc.UnaryStreamClientInterceptor,
    grpc.StreamUnaryClientInterceptor,
    grpc.StreamStreamClientInterceptor,
):
    def __init__(self, metadata: Mapping[str, str]) -> None:
        self._metadata = dict(metadata)

    def _intercept(
        self, continuation: Callable[..., Any], details: Any, request: Any
    ) -> Any:
        return continuation(_ClientCallDetails(details, self._metadata), request)

    def intercept_unary_unary(
        self, continuation: Callable[..., Any], client_call_details: Any, request: Any
    ) -> Any:
        return self._intercept(continuation, client_call_details, request)

    def intercept_unary_stream(
        self, continuation: Callable[..., Any], client_call_details: Any, request: Any
    ) -> Any:
        return self._intercept(continuation, client_call_details, request)

    def intercept_stream_unary(
        self, continuation: Callable[..., Any], client_call_details: Any, request_iterator: Any
    ) -> Any:
        return self._intercept(continuation, client_call_details, request_iterator)

    def intercept_stream_stream(
        self, continuation: Callable[..., Any], client_call_details: Any, request_iterator: Any
    ) -> Any:
        return self._intercept(continuation, client_call_details, request_iterator)


class BearerTokenInterceptor(MetadataInterceptor):
    def __init__(self, authorization: str) -> None:
        super().__init__({"authorization": authorization})


class _AsyncMetadataInterceptor:
    def __init__(self, metadata: Mapping[str, str]) -> None:
        self._metadata = dict(metadata)

    async def _intercept(
        self, continuation: Callable[..., Any], details: Any, request: Any
    ) -> Any:
        authenticated = details._replace(
            metadata=_metadata(details.metadata, self._metadata)
        )
        return await continuation(authenticated, request)


# grpc.aio registers an interceptor under the first matching RPC interface,
# so each shape needs a separate instance instead of one multi-interface class.
class _AsyncUnaryUnary(_AsyncMetadataInterceptor, grpc.aio.UnaryUnaryClientInterceptor):
    async def intercept_unary_unary(
        self, continuation: Callable[..., Any], client_call_details: Any, request: Any
    ) -> Any:
        return await self._intercept(continuation, client_call_details, request)


class _AsyncUnaryStream(_AsyncMetadataInterceptor, grpc.aio.UnaryStreamClientInterceptor):
    async def intercept_unary_stream(
        self, continuation: Callable[..., Any], client_call_details: Any, request: Any
    ) -> Any:
        return await self._intercept(continuation, client_call_details, request)


class _AsyncStreamUnary(_AsyncMetadataInterceptor, grpc.aio.StreamUnaryClientInterceptor):
    async def intercept_stream_unary(
        self, continuation: Callable[..., Any], client_call_details: Any, request_iterator: Any
    ) -> Any:
        return await self._intercept(continuation, client_call_details, request_iterator)


class _AsyncStreamStream(_AsyncMetadataInterceptor, grpc.aio.StreamStreamClientInterceptor):
    async def intercept_stream_stream(
        self, continuation: Callable[..., Any], client_call_details: Any, request_iterator: Any
    ) -> Any:
        return await self._intercept(continuation, client_call_details, request_iterator)


def async_metadata_interceptors(metadata: Mapping[str, str]) -> tuple[grpc.aio.ClientInterceptor, ...]:
    return (
        _AsyncUnaryUnary(metadata),
        _AsyncUnaryStream(metadata),
        _AsyncStreamUnary(metadata),
        _AsyncStreamStream(metadata),
    )


def async_auth_interceptors(authorization: str) -> tuple[grpc.aio.ClientInterceptor, ...]:
    return async_metadata_interceptors({"authorization": authorization})
