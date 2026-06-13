import asyncio
import time
import pytest

from agentic_sandbox_client import AsyncSandboxClient


@pytest.mark.asyncio
async def test_concurrent_sandbox_creation():
    
    client = AsyncSandboxClient()

    async def create(name):
        return await client.create_sandbox(name=name)

    start = time.time()

    await asyncio.gather(
        create("async-sandbox-1"),
        create("async-sandbox-2"),
        create("async-sandbox-3"),
    )

    end = time.time()
    total_time = end - start

    # Keep threshold relaxed for CI environments
    assert total_time < 30, f"Expected concurrent execution, got {total_time}s"


@pytest.mark.asyncio
async def test_event_loop_not_blocked():
    
    client = AsyncSandboxClient()

    async def create():
        await client.create_sandbox(name="async-sandbox-event-loop")

    async def dummy():
        await asyncio.sleep(1)
        return "done"

    start = time.time()

    results = await asyncio.gather(
        create(),
        dummy(),
    )

    end = time.time()

    assert "done" in results
    assert end - start < 10, "Event loop appears to be blocked"


@pytest.mark.asyncio
async def test_multiple_async_operations():
    
    client = AsyncSandboxClient()

    async def workflow(name):
        sandbox = await client.create_sandbox(name=name)
        await asyncio.sleep(0.5)
        return sandbox

    results = await asyncio.gather(
        workflow("async-s1"),
        workflow("async-s2"),
        workflow("async-s3"),
    )

    assert len(results) == 3