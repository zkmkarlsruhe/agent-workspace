# SPDX-License-Identifier: MIT

import asyncio
import json
import resource
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import server


class Request:
    def __init__(self, disconnect=False):
        self.disconnect = disconnect
        self.headers = {"Authorization": "Bearer test-secret"}

    async def stream(self):
        yield b'{}'

    async def is_disconnected(self):
        return self.disconnect


class TransportTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        secret = patch.object(server, "SECRET", "test-secret")
        secret.start()
        self.addCleanup(secret.stop)

    async def test_auth_before_read_or_spawn(self):
        request = Request()
        for auth in ("", "Bearer wrong", "Bearer ü"):
            request.headers["Authorization"] = auth
            with patch.object(request, "stream", side_effect=AssertionError("read before auth")), \
                 patch.object(server.asyncio, "create_subprocess_exec", side_effect=AssertionError("spawn before auth")):
                self.assertEqual((await server.process(request)).status_code, 401)
        with patch.object(server, "SECRET", ""):
            request.headers["Authorization"] = "Bearer "
            self.assertEqual((await server.process(request)).status_code, 401)

    async def test_child_address_space_limit(self):
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "worker.py").write_text(
                "import json, resource\n"
                "limit = resource.getrlimit(resource.RLIMIT_AS)[0]\n"
                "try: bytearray(limit * 2)\n"
                "except MemoryError: print(json.dumps({'limited': limit}))\n"
                "else: raise RuntimeError('unbounded child')\n")
            with patch.object(server, "__file__", str(Path(directory, "server.py"))), \
                 patch.object(server, "CHILD_MEMORY", 64 << 20):
                response = await server.process(Request())
            self.assertEqual(response.status_code, 200)
            self.assertEqual(json.loads(response.body), {"limited": 64 << 20})
        self.assertNotEqual(resource.getrlimit(resource.RLIMIT_AS)[0], 64 << 20)

    async def test_invalid_worker_response(self):
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "worker.py").write_text("print('truncated json')")
            with patch.object(server, "__file__", str(Path(directory, "server.py"))):
                self.assertEqual((await server.process(Request())).status_code, 422)

    async def test_busy_and_upload_limit(self):
        async with server.slot:
            self.assertEqual((await server.process(Request())).status_code, 429)
        with patch.object(server, "MAX_REQUEST", 1):
            self.assertEqual((await server.process(Request())).status_code, 413)
        self.assertFalse(server.slot.locked())

    async def test_disconnect_and_timeout_kill_worker(self):
        spawn = asyncio.create_subprocess_exec
        children = []

        async def tracked(*args, **kwargs):
            child = await spawn(*args, **kwargs)
            children.append(child)
            return child

        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "worker.py").write_text("import sys,time; sys.stdin.buffer.read(); time.sleep(30)")
            with patch.object(server, "__file__", str(Path(directory, "server.py"))), \
                 patch.object(server.asyncio, "create_subprocess_exec", tracked), \
                 patch.object(server, "PROCESS_TIMEOUT", 0.05):
                self.assertEqual((await server.process(Request(True))).status_code, 499)
                self.assertEqual((await server.process(Request())).status_code, 504)
        self.assertEqual(len(children), 2)
        self.assertTrue(all(child.returncode is not None for child in children))
        self.assertFalse(server.slot.locked())


if __name__ == "__main__":
    unittest.main()
