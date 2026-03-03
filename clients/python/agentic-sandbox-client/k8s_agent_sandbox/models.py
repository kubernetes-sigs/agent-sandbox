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

from typing import Literal
from pydantic import BaseModel

class ExecutionResult(BaseModel):
    """A structured object for holding the result of a command execution."""
    stdout: str = ""  # Standard output from the command.
    stderr: str = ""  # Standard error from the command.
    exit_code: int = -1  # Exit code of the command.

class FileEntry(BaseModel):
    """Represents a file or directory entry in the sandbox."""
    name: str # Name of the file.
    size: int  # Size of the file in bytes.
    type: Literal["file", "directory"]  # Type of the entry (file or directory).
    mod_time: float # Last modification time of the file. (POSIX timestamp)
