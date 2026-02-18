# Copyright 2025 The Kubernetes Authors.
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
"""
This module manages OpenTelemetry tracing integration for the Agentic Sandbox client.
It provides a wrapper for the OpenTelemetry SDK to handle tracing initialization,
span creation, and context propagation. If OpenTelemetry is not installed, it
falls back to no-op mock objects.
"""

import atexit
import functools
import json
import logging
import threading
from contextlib import nullcontext

# If optional dependency OpenTelemetry is not installed, define a complete set of mock objects
# to prevent runtime errors.
try:
    from opentelemetry import trace, context
    from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor
    from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
    OPENTELEMETRY_AVAILABLE = True
except ImportError:
    OPENTELEMETRY_AVAILABLE = False
    logging.debug("OpenTelemetry not installed; using MockTracer.")

    class MockSpan:
        """Mock class for OpenTelemetry Span."""

        def is_recording(self):
            """Mock is_recording."""
            return False

        def set_attribute(self, key, value):
            """Mock set_attribute."""

        def end(self):
            """Mock end."""

    class MockTracer:
        """Mock class for OpenTelemetry Tracer."""

        def start_as_current_span(self, *args, **kwargs):
            """Mock start_as_current_span."""
            return nullcontext()

        def start_span(self, *args, **kwargs):
            """Mock start_span."""
            return MockSpan()

    class TraceStub:
        """Mock class for OpenTelemetry trace module."""
        @staticmethod
        def get_current_span():
            """Mock get_current_span."""
            return MockSpan()

        @staticmethod
        def set_tracer_provider(_):
            """Mock set_tracer_provider."""
        @staticmethod
        def get_tracer(name, version=None):
            """Mock get_tracer."""
            return MockTracer()

        @staticmethod
        def set_span_in_context(span, context=None):
            """Mock set_span_in_context."""

    class TraceContextTextMapPropagator:
        """Mock class for OpenTelemetry TraceContextTextMapPropagator."""

        def inject(self, carrier, context=None, setter=None):
            """Mock inject."""

    class ContextStub:
        """Mock class for OpenTelemetry context module."""
        @staticmethod
        def attach(*args, **kwargs):
            """Mock attach."""

        @staticmethod
        def detach(*args, **kwargs):
            """Mock detach."""

    # Assign mock stubs to match import names
    trace = TraceStub
    context = ContextStub

# --- Global state for the singleton TracerProvider ---
_TRACER_PROVIDER = None
_TRACER_PROVIDER_LOCK = threading.Lock()


def initialize_tracer(service_name: str):
    """
    Initializes the global OpenTelemetry TracerProvider using the singleton pattern.

    This function uses double-checked locking to ensure thread-safe, one-time initialization.

    Behavior:
    - If OpenTelemetry is not installed, this is a no-op.
    - If the Provider is already initialized, it verifies that the requested 'service_name'
      matches the existing global service name. If they differ, a warning is logged
      indicating that the requested name will be ignored in favor of the existing one.
    - Configures a BatchSpanProcessor and OTLPSpanExporter for sending traces.
    """
    global _TRACER_PROVIDER

    if not OPENTELEMETRY_AVAILABLE:
        logging.error(
            "OpenTelemetry not installed; skipping tracer initialization.")
        return

    # First check (no lock) for performance.
    if _TRACER_PROVIDER is not None:
        try:
            existing_name = _TRACER_PROVIDER.resource.attributes.get(
                "service.name")
            if existing_name and existing_name != service_name:
                logging.warning(
                    f"Global TracerProvider already initialized with service name '{existing_name}'. "
                    f"Ignoring request to initialize with '{service_name}'."
                )
        except Exception:
            # Fallback if accessing attributes fails for any reason
            pass
        return

    with _TRACER_PROVIDER_LOCK:
        # Second check (with lock) to ensure thread safety.
        if _TRACER_PROVIDER is None:
            resource = Resource(attributes={"service.name": service_name})
            _TRACER_PROVIDER = TracerProvider(resource=resource)
            _TRACER_PROVIDER.add_span_processor(
                BatchSpanProcessor(OTLPSpanExporter())
            )
            trace.set_tracer_provider(_TRACER_PROVIDER)
            # Ensure shutdown is called only once when the process exits.
            atexit.register(_TRACER_PROVIDER.shutdown)
            logging.info(
                f"Global OpenTelemetry TracerProvider configured for service '{service_name}'.")


def trace_span(span_suffix):
    """
    Decorator to wrap a method in an OpenTelemetry span with a dynamic name.

    The final span name is constructed at runtime as:
        "{self.trace_service_name}.{span_suffix}"

    The decorated method must belong to an instance (`self`) that has:
        1. `self.tracer`: An initialized OpenTelemetry Tracer.
        2. `self.trace_service_name`: The string name of the service (e.g., 'sandbox-client').

    If `self.tracer` is None (tracing disabled), the method runs without decoration.
    """
    def decorator(func):
        @functools.wraps(func)
        def wrapper(self, *args, **kwargs):
            tracer = getattr(self, 'tracer', None)
            if not tracer:
                return func(self, *args, **kwargs)

            # Determine the service name at runtime
            service_name = getattr(
                self, 'trace_service_name', 'sandbox-client')
            span_name = f"{service_name}.{span_suffix}"

            with tracer.start_as_current_span(span_name):
                return func(self, *args, **kwargs)
        return wrapper
    return decorator


class TracerManager:
    """
    Manages the tracing lifecycle for a single client instance.

    This manager isolates the client's tracing context by:
    1. Creating a tracer with a unique 'instrumentation scope' based on the client's name.
    2. Managing a 'lifecycle' span that serves as the parent for all operations.
    3. Handling the attachment/detachment of the OTel context to the current thread.
    """

    def __init__(self, service_name: str):
        instrumentation_scope_name = service_name.replace('-', '_')
        self.tracer = trace.get_tracer(instrumentation_scope_name)
        self.lifecycle_span_name = f"{service_name}.lifecycle"
        self.parent_span = None
        self.context_token = None
        self.propagator = TraceContextTextMapPropagator()

    def start_lifecycle_span(self):
        """Starts the main parent span for the client's lifecycle."""
        if not self.tracer:
            return

        self.parent_span = self.tracer.start_span(self.lifecycle_span_name)
        ctx = trace.set_span_in_context(self.parent_span)
        self.context_token = context.attach(ctx)

    def end_lifecycle_span(self):
        """Ends the main parent span and detaches the context."""
        if self.context_token:
            context.detach(self.context_token)
        if self.parent_span:
            self.parent_span.end()

    def get_trace_context_json(self) -> str:
        """Captures only traceparent and tracestate (excludes baggage)."""
        carrier = {}
        self.propagator.inject(carrier)
        return json.dumps(carrier) if carrier else ""
