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

FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY main.py .

# Prepare a workspace directory the runtime can safely confine file I/O to.
RUN mkdir -p /workspace && chown -R 1000:1000 /app /workspace

USER 1000

# The sandbox runtime API port.
EXPOSE 8888

ENV SANDBOX_WORKSPACE=/workspace

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8888"]
