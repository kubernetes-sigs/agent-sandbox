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

import asyncio
import logging
from jupyter_client import AsyncKernelManager

class SandboxKernelManager:
    """Manages the lifecycle and execution of the IPython kernel."""

    def __init__(self):
        self.kernel_manager = AsyncKernelManager(kernel_name='python3')
        self.kernel_client = None
        self.lock = asyncio.Lock()

    async def start(self):
        """Starts or restarts the kernel."""
        # 1. Ensure any existing client channels are stopped before restarting.
        if self.kernel_client:
            self.kernel_client.stop_channels()

        if self.kernel_manager.has_kernel:
            await self.kernel_manager.shutdown_kernel(now=True)
         
        # 2. Start the kernel and open the channels for the client to listen.   
        await self.kernel_manager.start_kernel()
        self.kernel_client = self.kernel_manager.client()
        self.kernel_client.start_channels()
        
        # 3. Check if the kernel client is ready.
        for attempt in range(5):
            try:
                await self.kernel_client.wait_for_ready(timeout=5)
                self.kernel_client.execute("import os, pickle; os.chdir('/app')")
                return
            except Exception:
                # If the kernel process is dead, stop waiting immediately.
                check = self.kernel_manager.is_alive()
                if not (await check if asyncio.iscoroutine(check) else check):
                    raise RuntimeError("Kernel process died unexpectedly during startup.")

                logging.warning(f"Kernel start failed (attempt {attempt + 1}). Retrying...")
                await asyncio.sleep(1)

        raise RuntimeError("Failed to start kernel after 5 attempts")

    async def shutdown(self):
        """Shuts down the kernel."""
        if self.kernel_client:
            self.kernel_client.stop_channels()
        if self.kernel_manager:
            await self.kernel_manager.shutdown_kernel()

    async def interrupt_and_drain(self):
        """Interrupts the kernel and drains the message queue."""
        if not self.kernel_manager or not self.kernel_client:
            return
        
        try:
            # 1. Trigger the interrupt
            await self.kernel_manager.interrupt_kernel()
            
            # 2. Drain with a deadline.
            async with asyncio.timeout(5.0):  # Total max time to wait for idle
                while True:
                    try:
                        # Short timeout per message to check if channel is empty
                        msg = await self.kernel_client.get_iopub_msg(timeout=0.2)       
                        if (msg['header']['msg_type'] == 'status' and 
                            msg['content']['execution_state'] == 'idle'):
                            break
                    except (asyncio.TimeoutError, Exception):
                        break

        except (asyncio.TimeoutError, Exception) as e:
            # 3. Force restart in case interruption / drain failed.
            logging.warning(f"Kernel drain failed or timed out: {e}. Forcing restart.")
            await self.start()

    async def execute(self, code: str, timeout: int) -> tuple[str, str, int]:
        """Executes code in the kernel and returns stdout, stderr, and exit code."""
        async with self.lock:
            # 1. Self-healing check: If client is missing or dead, try to re-start the kernel again.
            if not self.kernel_client or not self.kernel_manager.has_kernel:
                logging.warning("Kernel not initialized. Attempting to start...")
                await self.start()

            # 2. Check if Kernel is alive before executing the code.
            is_alive = False
            try:
                check = self.kernel_manager.is_alive()
                is_alive = await check if asyncio.iscoroutine(check) else check
            except Exception:
                pass
            
            if not is_alive:
                logging.warning("Kernel is dead. Restarting...")
                await self.start()

            # 3. Flush the IOPub channel before starting
            try:
                while True:
                    await asyncio.wait_for(self.kernel_client.get_iopub_msg(), timeout=0.01)
            except (asyncio.TimeoutError, Exception):
                pass

            # 4. Execute the requested code.
            msg_id = self.kernel_client.execute(code)
            stdout, stderr = [], []
            exit_code = 0

            async def collect_io():
                nonlocal exit_code
                while True:
                    msg = await self.kernel_client.get_iopub_msg()
                    if msg.get('parent_header', {}).get('msg_id') != msg_id:
                        continue

                    msg_type = msg['header']['msg_type']
                    content = msg['content']

                    if msg_type == 'stream':
                        if content['name'] == 'stdout':
                            stdout.append(content['text'])
                        else:
                            stderr.append(content['text'])
                    elif msg_type in ('execute_result', 'display_data'):
                        data = content['data'].get('text/plain', '')
                        stdout.append(data)
                    elif msg_type == 'error':
                        stderr.append("\n".join(content['traceback']))
                    elif msg_type == 'status' and content['execution_state'] == 'idle':
                        break

            try:
                # 5. Collect the messages from IOPub and Shell channel and return to the user.
                await asyncio.wait_for(collect_io(), timeout=timeout)
                
                shell_msg = await asyncio.wait_for(self.kernel_client.get_shell_msg(), timeout=1.0)
                if shell_msg['content'].get('status') == 'error':
                    exit_code = 1
                
                return "".join(stdout), "".join(stderr), exit_code

            except asyncio.TimeoutError:
                await self.interrupt_and_drain()
                return "".join(stdout), "".join(stderr) + f"\n[Timeout after {timeout}s]", 130
