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

import json
import urllib.error
from unittest import mock

from chat_hermes import chat_with_hermes


def _mock_response(status, body):
    response = mock.MagicMock()
    response.status = status
    response.read.return_value = json.dumps(body).encode()
    response.__enter__.return_value = response
    return response


@mock.patch("chat_hermes.urllib.request.urlopen")
def test_chat_with_hermes_returns_message_content(mock_urlopen):
    mock_urlopen.return_value = _mock_response(
        200, {"choices": [{"message": {"content": "hello there"}}]}
    )

    assert chat_with_hermes("hi") == "hello there"


@mock.patch("chat_hermes.urllib.request.urlopen")
def test_chat_with_hermes_reports_non_200_status(mock_urlopen):
    mock_urlopen.return_value = _mock_response(500, {})

    assert chat_with_hermes("hi") == "Error: Received status code 500"


@mock.patch("chat_hermes.urllib.request.urlopen")
def test_chat_with_hermes_reports_connection_failure(mock_urlopen):
    mock_urlopen.side_effect = urllib.error.URLError("connection refused")

    result = chat_with_hermes("hi")

    assert "Connection failed" in result
    assert "Is port-forwarding running?" in result
