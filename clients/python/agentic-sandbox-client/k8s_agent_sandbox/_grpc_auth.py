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

"""Channel-level sandboxd authentication, imported only when gRPC is used.

Interceptors add credentials without consuming streams or changing RPC
lifetimes, deadlines, cancellation, or error handling.
"""

from collections.abc import Callable, Iterable
from typing import Any

import grpc


def _metadata(
    original: Iterable[tuple[str, str | bytes]] | None, authorization: str
) -> tuple[tuple[str, str | bytes], ...]:
    # Preserve duplicate and binary application metadata, replacing only auth.
    return tuple(
        (key, value) for key, value in (original or ())
        if key.lower() != "authorization"
    ) + (("authorization", authorization),)


class _ClientCallDetails(grpc.ClientCallDetails):
    def __init__(self, original: Any, authorization: str) -> None:
        self.method = original.method
        self.timeout = original.timeout
        self.metadata = _metadata(original.metadata, authorization)
        self.credentials = original.credentials
        self.wait_for_ready = getattr(original, "wait_for_ready", None)
        self.compression = getattr(original, "compression", None)


class BearerTokenInterceptor(
    grpc.UnaryUnaryClientInterceptor,
    grpc.UnaryStreamClientInterceptor,
    grpc.StreamUnaryClientInterceptor,
    grpc.StreamStreamClientInterceptor,
):
    def __init__(self, authorization: str) -> None:
        self._authorization = authorization

    def _intercept(
        self, continuation: Callable[..., Any], details: Any, request: Any
    ) -> Any:
        return continuation(_ClientCallDetails(details, self._authorization), request)

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


class _AsyncBearerTokenInterceptor:
    def __init__(self, authorization: str) -> None:
        self._authorization = authorization

    async def _intercept(
        self, continuation: Callable[..., Any], details: Any, request: Any
    ) -> Any:
        authenticated = details._replace(
            metadata=_metadata(details.metadata, self._authorization)
        )
        return await continuation(authenticated, request)


# grpc.aio registers an interceptor under the first matching RPC interface,
# so each shape needs a separate instance instead of one multi-interface class.
class _AsyncUnaryUnary(_AsyncBearerTokenInterceptor, grpc.aio.UnaryUnaryClientInterceptor):
    async def intercept_unary_unary(
        self, continuation: Callable[..., Any], client_call_details: Any, request: Any
    ) -> Any:
        return await self._intercept(continuation, client_call_details, request)


class _AsyncUnaryStream(_AsyncBearerTokenInterceptor, grpc.aio.UnaryStreamClientInterceptor):
    async def intercept_unary_stream(
        self, continuation: Callable[..., Any], client_call_details: Any, request: Any
    ) -> Any:
        return await self._intercept(continuation, client_call_details, request)


class _AsyncStreamUnary(_AsyncBearerTokenInterceptor, grpc.aio.StreamUnaryClientInterceptor):
    async def intercept_stream_unary(
        self, continuation: Callable[..., Any], client_call_details: Any, request_iterator: Any
    ) -> Any:
        return await self._intercept(continuation, client_call_details, request_iterator)


class _AsyncStreamStream(_AsyncBearerTokenInterceptor, grpc.aio.StreamStreamClientInterceptor):
    async def intercept_stream_stream(
        self, continuation: Callable[..., Any], client_call_details: Any, request_iterator: Any
    ) -> Any:
        return await self._intercept(continuation, client_call_details, request_iterator)


def async_auth_interceptors(authorization: str) -> tuple[grpc.aio.ClientInterceptor, ...]:
    return (
        _AsyncUnaryUnary(authorization),
        _AsyncUnaryStream(authorization),
        _AsyncStreamUnary(authorization),
        _AsyncStreamStream(authorization),
    )
