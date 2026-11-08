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

"""Verify a JupyterLab sandbox serves correctly under the router's browser prefix.

Run it against the sandbox-router (see README.md for the port-forward step):

    BASE_URL=http://127.0.0.1:8080 \
    PREFIX=/sandboxes/default/jupyterlab-base-path/8888 \
      python3 verify.py

PREFIX must match the sandbox's base_url in sandbox.yaml. Prints PASS/FAIL per
check and exits non-zero on any FAIL.
"""
import http.client
import http.cookiejar
import json
import os
import re
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("BASE_URL", "http://127.0.0.1:8080")
PREFIX = os.environ.get("PREFIX", "/sandboxes/default/jupyterlab-base-path/8888")

results = []


def check(name, ok, detail=""):
    ok = bool(ok)
    results.append(ok)
    print(("PASS " if ok else "FAIL ") + name + (("  -- " + detail) if detail else ""))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


opener_noredir = urllib.request.build_opener(NoRedirect)
jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))


def print_summary_and_exit():
    print()
    print(f"{sum(results)}/{len(results)} checks passed")
    sys.exit(1)


def report_unreachable(url, err):
    # Connection refused, reset, timeout, or a truncated response: report it
    # as a failed check that names the step, instead of a traceback.
    check("reach " + BASE + url, False, repr(getattr(err, "reason", err)))
    print_summary_and_exit()


def get(url, op=opener):
    try:
        r = op.open(BASE + url, timeout=20)
        return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        # HTTPError is a URLError and an OSError, so it must be caught first.
        try:
            body = e.read()
        except (OSError, http.client.HTTPException) as read_err:
            report_unreachable(url, read_err)
        return e.code, dict(e.headers), body
    except (OSError, http.client.HTTPException) as e:
        report_unreachable(url, e)


# 1. Root without prefix must not serve the app (prefix is load-bearing).
st, _, _ = get("/lab")
check("unprefixed /lab is not served by the app", st != 200 and st < 500, f"status={st}")

# 2. A real redirect: prefix without trailing slash must redirect to a prefixed Location.
st, h, _ = get(PREFIX, op=opener_noredir)
loc = h.get("Location", "")
check("prefix without slash redirects to a prefixed Location",
      st in (301, 302, 307, 308) and loc.startswith(PREFIX),
      f"status={st} Location={loc!r}")

# 3. Lab HTML loads and its asset/API URLs are under the prefix.
st, h, body = get(PREFIX + "/lab")
html = body.decode("utf-8", "replace")
check("GET <prefix>/lab returns 200", st == 200, f"status={st}")
base_url_set = (f'"baseUrl": "{PREFIX}/"' in html
                or f'baseUrl":"{PREFIX}/' in html.replace(" ", ""))
check("page config baseUrl is the prefix", base_url_set, "")
static_refs = re.findall(r'(?:src|href)="(/[^"]*static[^"]*)"', html)
bad_static = [s for s in static_refs if not s.startswith(PREFIX)]
check("static asset refs in HTML are prefixed",
      len(static_refs) > 0 and not bad_static,
      f"{len(static_refs)} refs, unprefixed={bad_static[:3]}")

# 4. Kernelspec logo URLs (the example from #1109) are prefixed.
st, _, body = get(PREFIX + "/api/kernelspecs")
logos = []
try:
    ks = json.loads(body)
    for name, spec in ks.get("kernelspecs", {}).items():
        res = spec.get("resources", {})
        logos += [v for v in res.values() if isinstance(v, str)]
except (ValueError, AttributeError) as e:
    # Not the kernelspec JSON we expected; leave logos empty so the check fails.
    print(f"kernelspecs response was not JSON: {e!r}", file=sys.stderr)
check("kernelspec resource URLs are prefixed",
      st == 200 and logos and all(l.startswith(PREFIX) for l in logos),
      f"status={st} n={len(logos)} sample={logos[:1]}")

# 5. Cookies set by the app are path-scoped under the prefix.
cookies = list(jar)
has_xsrf = any(c.name == "_xsrf" for c in cookies)
check("app set an _xsrf cookie (so the scoping check below means something)", has_xsrf,
      "cookies=" + ",".join(f"{c.name}@{c.path}" for c in cookies))
check("app-set cookies are scoped to the prefix",
      has_xsrf and all((c.path or "").startswith(PREFIX) for c in cookies),
      "cookies=" + ",".join(f"{c.name}@{c.path}" for c in cookies))

# 6. Kernel lifecycle + execution via prefixed REST and WebSocket.
xsrf_val = next((c.value for c in jar if c.name == "_xsrf"), "")
hdrs = {"Content-Type": "application/json", "X-XSRFToken": xsrf_val}
req = urllib.request.Request(BASE + PREFIX + "/api/kernels",
                             data=json.dumps({"name": "python3"}).encode(),
                             headers=hdrs, method="POST")
try:
    kr = json.loads(opener.open(req, timeout=60).read())
    kid = kr["id"]
    check("kernel started via prefixed REST", True, f"id={kid}")
except Exception as e:
    kid = None
    check("kernel started via prefixed REST", False, repr(e))

if kid:
    try:
        import websocket  # websocket-client
        ws_url = BASE.replace("http", "ws") + f"{PREFIX}/api/kernels/{kid}/channels"
        ws = websocket.create_connection(ws_url, timeout=30)
        msg_id = "basepath-check-1"
        ws.send(json.dumps({
            "header": {"msg_id": msg_id, "username": "", "session": "s1",
                       "msg_type": "execute_request", "version": "5.3"},
            "parent_header": {}, "metadata": {},
            "content": {"code": "print(6*7)", "silent": False},
            "buffers": [], "channel": "shell"}))
        out = None
        for _ in range(50):
            m = json.loads(ws.recv())
            if m.get("parent_header", {}).get("msg_id") == msg_id and m["header"]["msg_type"] == "stream":
                out = m["content"]["text"].strip()
                break
        ws.close()
        check("kernel WebSocket under prefix executes code", out == "42", f"output={out!r}")
    except Exception as e:
        check("kernel WebSocket under prefix executes code", False, repr(e))
    finally:
        req = urllib.request.Request(BASE + PREFIX + f"/api/kernels/{kid}", headers=hdrs, method="DELETE")
        try:
            opener.open(req, timeout=20)
        except Exception:
            pass

print()
print(f"{sum(results)}/{len(results)} checks passed")
sys.exit(0 if all(results) else 1)
