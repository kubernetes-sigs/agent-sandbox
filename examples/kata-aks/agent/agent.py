# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

"""Per-owner agent backed by OpenAI-compatible endpoint.

Each pod is identical; the per-user identity comes from the X-Owner header
on the incoming request, so a single warm-pool of pods can serve every
user without any per-pod customization.
"""

import os
from collections import defaultdict, deque
from threading import Lock

from fastapi import FastAPI, Header
from openai import OpenAI
from pydantic import BaseModel

# OPENAI_BASE_URL must point at the OpenAI-compatible v1 endpoint,
# e.g. https://<resource>.openai.azure.com/openai/v1/ .
_base_url = os.getenv("OPENAI_BASE_URL")
_api_key = os.getenv("OPENAI_API_KEY")
_model = os.getenv("LLM_MODEL")

_missing_settings = [
    name
    for name, value in (
        ("OPENAI_BASE_URL", _base_url),
        ("OPENAI_API_KEY", _api_key),
        ("LLM_MODEL", _model),
    )
    if not value
]
if _missing_settings:
    raise RuntimeError(
        "Missing required environment setting(s): "
        + ", ".join(_missing_settings)
    )

_client = OpenAI(
    base_url=_base_url,
    api_key=_api_key,
)

# In-process per-owner chat history. A bounded deque keeps the prompt
# size predictable on long conversations; bump _HISTORY_TURNS if you
# need more context. History lives in memory only — restarting the pod
# wipes it, which is fine for a demo sandbox.
_HISTORY_TURNS = 20  # user+assistant message pairs per owner
_history: dict[str, deque] = defaultdict(lambda: deque(maxlen=_HISTORY_TURNS * 2))
_history_lock = Lock()

app = FastAPI()


class ChatRequest(BaseModel):
    prompt: str


@app.get("/healthz")
def healthz() -> dict:
    return {"ok": True}


@app.post("/reset")
def reset(x_owner: str = Header(default="anonymous")) -> dict:
    """Drop the conversation history for this owner."""
    with _history_lock:
        _history.pop(x_owner, None)
    return {"owner": x_owner, "reset": True}


@app.post("/chat")
def chat(body: ChatRequest, x_owner: str = Header(default="anonymous")) -> dict:
    system = (
        f"You are {x_owner}'s personal AI agent running in a private sandbox. "
        f"Always begin your reply with 'I am {x_owner}'s agent.' "
        f"Never claim to belong to anyone else. Keep replies under three sentences."
    )

    with _history_lock:
        prior = list(_history[x_owner])

    messages = [{"role": "system", "content": system}]
    messages.extend(prior)
    messages.append({"role": "user", "content": body.prompt})

    resp = _client.chat.completions.create(model=_model, messages=messages)
    reply = resp.choices[0].message.content

    with _history_lock:
        h = _history[x_owner]
        h.append({"role": "user", "content": body.prompt})
        h.append({"role": "assistant", "content": reply})

    return {"owner": x_owner, "reply": reply, "history_turns": len(prior) // 2 + 1}
