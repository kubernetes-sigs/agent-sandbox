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

"""Bounded names and label values."""

from k8s_agent_sandbox_harbor.labels import label_value, object_name, short_hash


def test_label_value_keeps_valid_values_and_hashes_the_rest():
    assert label_value("hello-world__bZZeEkw__env") == "hello-world__bZZeEkw__env"
    long = "x" * 80
    bounded = label_value(long)
    assert len(bounded) == 63 and bounded.endswith(short_hash(long))
    odd = label_value("task with spaces/and slashes")
    assert odd.startswith("task-with-spaces-and-slashes-") and len(odd) <= 63
    assert label_value("-leading") != "-leading" and label_value("") == ""
    assert label_value(long) == label_value(long)             # stable
    assert label_value("a" * 80) != label_value("a" * 81)     # distinct


def test_object_name_is_dns_safe_and_stable():
    name = object_name("hb", "alexgshaw/x:1", "{}")
    assert name == "hb-" + short_hash("alexgshaw/x:1", "{}") and len(name) <= 63
    assert object_name("Hb_Prefix!", "k") .startswith("hb-prefix-")
    assert object_name("", "k") == short_hash("k")
