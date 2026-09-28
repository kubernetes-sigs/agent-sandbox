# Copyright 2026 The Kubernetes Authors.
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

"""Tests for synchronous and asynchronous filesystem event streaming."""

import sys
import unittest
from unittest.mock import AsyncMock, MagicMock, patch


class TestFileWatcher(unittest.TestCase):
    """Tests for the sync FileWatcher."""

    @patch("k8s_agent_sandbox.watcher.file_watcher.trace")
    def test_watch_raises_on_legacy_runtime(self, mock_trace):
        """FileWatcher.watch should reject the legacy runtime."""
        from k8s_agent_sandbox.watcher.file_watcher import FileWatcher

        mock_span = MagicMock()
        mock_span.is_recording.return_value = False
        mock_trace.get_current_span.return_value = mock_span

        connector = MagicMock()
        connector.is_sandboxd.return_value = False

        watcher = FileWatcher(connector, MagicMock(), "test")
        with self.assertRaises(RuntimeError) as ctx:
            list(watcher.watch("."))
        self.assertIn("sandboxd runtime", str(ctx.exception))

    @patch("k8s_agent_sandbox.watcher.file_watcher.trace")
    def test_watch_yields_sdk_events(self, mock_trace):
        """FileWatcher.watch should translate proto events to SDK FileEvent."""
        from k8s_agent_sandbox.watcher.file_watcher import FileWatcher
        from types import SimpleNamespace

        # Reset the event-type cache so it is rebuilt with our mock values.
        import k8s_agent_sandbox.watcher.file_watcher as _fw
        _fw._EVENT_TYPE_NAMES = None

        # Mock proto module: enum constants must be integers so the type
        # mapping produces real SDK strings.
        mock_pb2 = SimpleNamespace(
            FILE_EVENT_TYPE_CREATE=1,
            FILE_EVENT_TYPE_WRITE=2,
            FILE_EVENT_TYPE_REMOVE=3,
            FILE_EVENT_TYPE_RENAME=4,
            FILE_EVENT_TYPE_CHMOD=5,
            FILE_EVENT_TYPE_ERROR=6,
            WatchDirRequest=MagicMock(),
        )

        mock_span = MagicMock()
        mock_span.is_recording.return_value = False
        mock_trace.get_current_span.return_value = mock_span

        # Proto events.
        proto_event = MagicMock()
        proto_event.type = 1  # FILE_EVENT_TYPE_CREATE
        proto_event.path = "hello.txt"
        proto_event.old_path = ""
        proto_event.error = ""

        # Mock stub.
        mock_stub_instance = MagicMock()
        mock_stub_instance.WatchDir.return_value = [proto_event]
        mock_grpc = SimpleNamespace(
            FileWatcherServiceStub=MagicMock(return_value=mock_stub_instance)
        )

        # Mock connector.
        connector = MagicMock()
        connector.is_sandboxd.return_value = True
        connector.grpc_channel.return_value = MagicMock()

        watcher = FileWatcher(connector, MagicMock(), "test")

        # Patch the entire _watcher_stubs module in sys.modules
        with patch.dict(sys.modules, {
            "k8s_agent_sandbox.watcher._watcher_stubs": SimpleNamespace(
                watcher_pb2=mock_pb2,
                watcher_pb2_grpc=mock_grpc,
            ),
            "grpc": SimpleNamespace(RpcError=Exception),
        }):
            events = list(watcher.watch("."))

        self.assertEqual(len(events), 1)
        self.assertEqual(events[0].type, "create")
        self.assertEqual(events[0].path, "hello.txt")
        connector.connect.assert_called_once()

    @patch("k8s_agent_sandbox.watcher.file_watcher.trace")
    def test_watch_passes_recursive_flag(self, mock_trace):
        """FileWatcher.watch should forward the recursive flag to the proto request."""
        from k8s_agent_sandbox.watcher.file_watcher import FileWatcher
        from types import SimpleNamespace

        # Reset event-type cache to avoid cross-test contamination.
        import k8s_agent_sandbox.watcher.file_watcher as _fw
        _fw._EVENT_TYPE_NAMES = None

        mock_span = MagicMock()
        mock_span.is_recording.return_value = False
        mock_trace.get_current_span.return_value = mock_span

        mock_pb2 = SimpleNamespace(
            FILE_EVENT_TYPE_CREATE=1,
            FILE_EVENT_TYPE_WRITE=2,
            FILE_EVENT_TYPE_REMOVE=3,
            FILE_EVENT_TYPE_RENAME=4,
            FILE_EVENT_TYPE_CHMOD=5,
            FILE_EVENT_TYPE_ERROR=6,
            WatchDirRequest=MagicMock(),
        )

        mock_stub_instance = MagicMock()
        mock_stub_instance.WatchDir.return_value = iter([])
        mock_grpc = SimpleNamespace(
            FileWatcherServiceStub=MagicMock(return_value=mock_stub_instance)
        )

        connector = MagicMock()
        connector.is_sandboxd.return_value = True
        connector.grpc_channel.return_value = MagicMock()

        watcher = FileWatcher(connector, MagicMock(), "test")

        # Patch the entire _watcher_stubs module in sys.modules
        with patch.dict(sys.modules, {
            "k8s_agent_sandbox.watcher._watcher_stubs": SimpleNamespace(
                watcher_pb2=mock_pb2,
                watcher_pb2_grpc=mock_grpc,
            ),
            "grpc": SimpleNamespace(RpcError=Exception),
        }):
            list(watcher.watch("src", recursive=True))

        # Verify the request had recursive=True.
        mock_pb2.WatchDirRequest.assert_called_once()
        call_kwargs = mock_pb2.WatchDirRequest.call_args.kwargs
        self.assertTrue(call_kwargs.get("recursive", False))


class TestAsyncFileWatcher(unittest.TestCase):
    """Tests for the async AsyncFileWatcher."""

    @patch("k8s_agent_sandbox.watcher.async_file_watcher.trace")
    def test_async_watch_raises_on_legacy_runtime(self, mock_trace):
        """AsyncFileWatcher.watch should reject the legacy runtime."""
        import asyncio
        from k8s_agent_sandbox.watcher.async_file_watcher import AsyncFileWatcher

        mock_span = MagicMock()
        mock_span.is_recording.return_value = False
        mock_trace.get_current_span.return_value = mock_span

        connector = MagicMock()
        connector.is_sandboxd.return_value = False

        watcher = AsyncFileWatcher(connector, MagicMock(), "test")

        async def _run():
            events = []
            async for ev in watcher.watch("."):
                events.append(ev)
            return events

        with self.assertRaises(RuntimeError) as ctx:
            asyncio.run(_run())
        self.assertIn("sandboxd runtime", str(ctx.exception))


class TestEventTypeMapping(unittest.TestCase):
    """Tests for the proto→SDK event type mapping."""

    def test_all_known_event_types_mapped(self):
        """Every proto FileEventType should map to a lowercase SDK string."""
        from k8s_agent_sandbox.watcher.file_watcher import _event_type_name

        # Use the actual proto constants via the shim.
        try:
            from k8s_agent_sandbox.watcher._watcher_stubs import watcher_pb2
        except ImportError:
            self.skipTest("grpcio not installed")

        self.assertEqual(_event_type_name(watcher_pb2.FILE_EVENT_TYPE_CREATE), "create")
        self.assertEqual(_event_type_name(watcher_pb2.FILE_EVENT_TYPE_WRITE), "write")
        self.assertEqual(_event_type_name(watcher_pb2.FILE_EVENT_TYPE_REMOVE), "remove")
        self.assertEqual(_event_type_name(watcher_pb2.FILE_EVENT_TYPE_RENAME), "rename")
        self.assertEqual(_event_type_name(watcher_pb2.FILE_EVENT_TYPE_CHMOD), "chmod")
        self.assertEqual(_event_type_name(watcher_pb2.FILE_EVENT_TYPE_ERROR), "error")

    def test_unknown_event_type(self):
        """An unknown enum value should produce a fallback string."""
        from k8s_agent_sandbox.watcher.file_watcher import _event_type_name

        # Skip if grpcio/protobuf not installed (required for _event_type_name initialization)
        try:
            from k8s_agent_sandbox.watcher._watcher_stubs import watcher_pb2  # noqa: F401
        except ImportError:
            self.skipTest("grpcio not installed")

        result = _event_type_name(999)
        self.assertIn("unknown", result)
        self.assertIn("999", result)


class TestFileEventModel(unittest.TestCase):
    """Tests for the FileEvent pydantic model."""

    def test_create_file_event(self):
        from k8s_agent_sandbox.models import FileEvent

        ev = FileEvent(type="create", path="hello.txt")
        self.assertEqual(ev.type, "create")
        self.assertEqual(ev.path, "hello.txt")
        self.assertEqual(ev.old_path, "")
        self.assertEqual(ev.error, "")

    def test_rename_event_with_old_path(self):
        from k8s_agent_sandbox.models import FileEvent

        ev = FileEvent(type="rename", path="new.txt", old_path="old.txt")
        self.assertEqual(ev.type, "rename")
        self.assertEqual(ev.old_path, "old.txt")

    def test_error_event(self):
        from k8s_agent_sandbox.models import FileEvent

        ev = FileEvent(type="error", error="watch limit reached")
        self.assertEqual(ev.type, "error")
        self.assertEqual(ev.error, "watch limit reached")


if __name__ == "__main__":
    unittest.main()
