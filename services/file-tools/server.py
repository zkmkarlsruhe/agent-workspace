# SPDX-License-Identifier: MIT

"""Internal-only transport; each transform runs in a killable subprocess."""

import asyncio
import json
import hmac
import os
import resource
import sys
from pathlib import Path

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)
slot = asyncio.Semaphore(1)
MAX_REQUEST = 29 << 20
PROCESS_TIMEOUT = 90
SECRET = os.environ.get("OC_FILE_TOOLS_SECRET", "")
CHILD_MEMORY = 10 << 30


def child_limits():
    resource.setrlimit(resource.RLIMIT_AS, (CHILD_MEMORY, CHILD_MEMORY))


@app.get("/health")
async def health():
    return {"status": "ok"}


@app.post("/process")
async def process(request: Request):
    if not SECRET or not hmac.compare_digest(request.headers.get("Authorization", "").encode(), ("Bearer " + SECRET).encode()):
        return JSONResponse({"error": "Unauthorized."}, status_code=401)
    if slot.locked():
        return JSONResponse({"error": "BUSY: Another file is being processed."}, status_code=429)
    async with slot:
        body = bytearray()
        try:
            async with asyncio.timeout(15):
                async for chunk in request.stream():
                    body.extend(chunk)
                    if len(body) > MAX_REQUEST:
                        return JSONResponse({"error": "Input exceeds transport limit."}, status_code=413)
        except TimeoutError:
            return JSONResponse({"error": "Upload timed out."}, status_code=408)
        child = await asyncio.create_subprocess_exec(
            sys.executable, str(Path(__file__).with_name("worker.py")),
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL, preexec_fn=child_limits)
        exchange = asyncio.create_task(child.communicate(body))
        try:
            async with asyncio.timeout(PROCESS_TIMEOUT):
                while not exchange.done():
                    if await request.is_disconnected():
                        return JSONResponse({"error": "Request disconnected."}, status_code=499)
                    await asyncio.wait({exchange}, timeout=0.2)
                output, _ = exchange.result()
            if child.returncode or len(output) > 60 << 20:
                return JSONResponse({"error": "Worker failed or exceeded output limit."}, status_code=422)
            try:
                result = json.loads(output)
            except (ValueError, UnicodeDecodeError):
                return JSONResponse({"error": "Invalid worker response."}, status_code=422)
            return JSONResponse(result, status_code=422 if "error" in result else 200)
        except TimeoutError:
            return JSONResponse({"error": "Processing stopped after 90 seconds. Reduce input size."}, status_code=504)
        finally:
            if child.returncode is None:
                child.kill()
            await child.wait()
            if not exchange.done():
                exchange.cancel()
            await asyncio.gather(exchange, return_exceptions=True)
