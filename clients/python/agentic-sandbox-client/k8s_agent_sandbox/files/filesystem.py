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

import json
import logging
import os
import urllib.parse
from typing import List
from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.models import FileEntry
from k8s_agent_sandbox.trace_manager import trace_span, trace

class Filesystem:
    """
    Handles file operations within the sandbox.
    """
    def __init__(self, connector: SandboxConnector, tracer, trace_service_name: str):
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name

    @trace_span("write")
    def write(self, path: str, content: bytes | str, timeout: int = 60):
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
            span.set_attribute("sandbox.file.size", len(content))

        if isinstance(content, str):
            content = content.encode('utf-8')

        filename = os.path.basename(path)
        files_payload = {'file': (filename, content)}
        self.connector.send_request("POST", "upload",
                      files=files_payload, timeout=timeout)
        logging.info(f"File '{filename}' uploaded successfully.")

    @trace_span("read")
    def read(self, path: str, timeout: int = 60) -> bytes:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)

        encoded_path = urllib.parse.quote(path, safe='')
        response = self.connector.send_request(
            "GET", f"download/{encoded_path}", timeout=timeout)
        content = response.content

        if span.is_recording():
            span.set_attribute("sandbox.file.size", len(content))

        return content

    @trace_span("list")
    def list(self, path: str, timeout: int = 60) -> List[FileEntry]:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        encoded_path = urllib.parse.quote(path, safe='')
        response = self.connector.send_request("GET", f"list/{encoded_path}", timeout=timeout)

        try:
            entries = response.json()
        except ValueError as e:
            raise RuntimeError(f"Failed to decode JSON response from sandbox: {response.text}") from e

        if not entries:
            return []

        try:
            file_entries = [FileEntry(**e) for e in entries]
        except Exception as e:
            raise RuntimeError(f"Server returned invalid file entry format: {entries}") from e

        if span.is_recording():
            span.set_attribute("sandbox.file.count", len(file_entries))
        return file_entries

    @trace_span("exists")
    def exists(self, path: str, timeout: int = 60) -> bool:
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        encoded_path = urllib.parse.quote(path, safe='')
        response = self.connector.send_request("GET", f"exists/{encoded_path}", timeout=timeout)
        
        try:
            response_data = response.json()
        except ValueError as e:
            raise RuntimeError(f"Failed to decode JSON response from sandbox: {response.text}") from e
            
        exists = response_data.get("exists", False)
        if span.is_recording():
            span.set_attribute("sandbox.file.exists", exists)
        return exists