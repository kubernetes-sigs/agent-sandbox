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

"""The report script reads Harbor result.json files and summarizes them."""

import importlib.util
import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("report", HERE / "scripts" / "report.py")
report = importlib.util.module_from_spec(spec)
assert spec.loader is not None
spec.loader.exec_module(report)


def _result(task, trial, reward=None, exc=None, setup=(0, 12.5)):
    t0 = "2026-10-30T10:00:00+00:00"
    t1 = "2026-10-30T10:00:12.500000+00:00"
    r = {"task_name": task, "trial_name": trial,
         "verifier_result": {"rewards": {"reward": reward}} if reward is not None else None,
         "environment_setup": {"started_at": t0, "finished_at": t1},
         "agent_execution": {"started_at": t0, "finished_at": "2026-10-30T10:01:00+00:00"},
         "verifier": None}
    if exc:
        r["exception_info"] = {"exception_type": exc, "exception_message": "x",
                               "exception_traceback": "", "occurred_at": t0}
    return r


def _write(job: Path, name: str, result: dict) -> None:
    (job / name).mkdir(parents=True)
    (job / name / "result.json").write_text(json.dumps(result))


def test_rows_and_summary(tmp_path: Path):
    job = tmp_path / "job"
    _write(job, "hello-world__a", _result("hello-world", "hello-world__a", reward=1))
    _write(job, "regex-log__b", _result("regex-log", "regex-log__b", reward=0))
    _write(job, "broken__c", _result("broken", "broken__c", exc="AgentTimeoutError"))
    rows = report.load_rows(job)
    assert [r["outcome"] for r in rows] == ["error: AgentTimeoutError", "pass", "fail"]
    assert rows[1]["env_setup_s"] == 12.5 and rows[1]["agent_s"] == 60.0 and rows[1]["verifier_s"] is None
    s = report.summary(rows)
    assert (s["trials"], s["passed"], s["errors"], s["pass_rate"]) == (3, 1, 1, 0.333)
    assert s["env_setup_p50_s"] == 12.5
    text = report.render(rows)
    assert "| hello-world | hello-world__a | 1 | pass | 12.5 | 60 | - |" in text
    assert text.rstrip().endswith("1/3 passed, 1 infrastructure errors, env setup p50 12.5 s / p95 12.5 s")


def test_main_handles_missing_and_json(tmp_path: Path, capsys):
    assert report.main([str(tmp_path / "nope")]) == 2
    assert report.main([str(tmp_path)]) == 1
    job = tmp_path / "job"
    _write(job, "t__1", _result("t", "t__1", reward=1))
    assert report.main([str(job), "--json"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["summary"]["passed"] == 1 and out["trials"][0]["task"] == "t"
    sys.stdout.flush()
