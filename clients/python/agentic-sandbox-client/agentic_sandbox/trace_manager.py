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

import functools
import logging
import atexit
import threading
from contextlib import nullcontext

# If optional dependency OpenTelemetry is not installed, define a complete set of mock objects
# to prevent runtime errors.
try:
    from opentelemetry import trace, propagate, context
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor
    from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.resources import Resource
    OPENTELEMETRY_AVAILABLE = True
except ImportError:
    OPENTELEMETRY_AVAILABLE = False
    logging.debug("OpenTelemetry not installed; using MockTracer.")
    class MockSpan:
        def is_recording(self): return False
        def set_attribute(self, key, value): pass
        def end(self): pass

    class MockTracer:
        def start_as_current_span(self, *args, **kwargs): return nullcontext()
        def start_span(self, *args, **kwargs): return MockSpan()

    class trace:
        @staticmethod
        def get_current_span(): return MockSpan()
        @staticmethod
        def set_tracer_provider(provider): pass
        @staticmethod
        def get_tracer(name, version=None): return MockTracer()
        @staticmethod
        def set_span_in_context(span, context=None): return None

    class propagate:
        @staticmethod
        def inject(*args, **kwargs): pass

    class context:
        @staticmethod
        def attach(*args, **kwargs): return None
        @staticmethod
        def detach(*args, **kwargs): pass

# --- Global state for the singleton TracerProvider ---
_TRACER_PROVIDER = None
_TRACER_PROVIDER_LOCK = threading.Lock()


def initialize_tracer(service_name: str):
    """Initializes and registers a global tracer provider using double-checked locking."""
    global _TRACER_PROVIDER, _TRACER_PROVIDER_LOCK
    # First check (no lock) for performance.
    if (not OPENTELEMETRY_AVAILABLE) or (_TRACER_PROVIDER is not None):
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


def trace_span(span_name):
    """A decorator to automatically wrap a method with an OpenTelemetry span."""
    def decorator(func):
        @functools.wraps(func)
        def wrapper(self, *args, **kwargs):
            tracer = getattr(self, 'tracer', None)
            if not tracer:
                return func(self, *args, **kwargs)

            with tracer.start_as_current_span(span_name):
                return func(self, *args, **kwargs)
        return wrapper
    return decorator


class TracerManager:
    """A lightweight manager for a single client's tracing lifecycle."""

    def __init__(self, service_name: str):
        instrumentation_scope_name = service_name.replace('-', '_')
        self.tracer = trace.get_tracer(instrumentation_scope_name)
        self.lifecycle_span_name = f"{service_name}.lifecycle"
        self.parent_span = None
        self.context_token = None

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
