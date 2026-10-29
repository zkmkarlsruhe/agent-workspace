# SPDX-License-Identifier: MIT

import io
import json
import unittest
from unittest.mock import patch

import mcp_stdio
import probe


class TransportTest(unittest.TestCase):
    def test_lost_response_never_replays_mutation(self):
        request = {"jsonrpc": "2.0", "id": 9, "method": "tools/call"}
        output = io.StringIO()
        with patch("sys.stdin", io.StringIO(json.dumps(request) + "\n")), \
                patch("sys.stdout", output), \
                patch("mcp_stdio.forward", side_effect=TimeoutError("timeout")) as send:
            mcp_stdio.main()
        self.assertEqual(send.call_count, 1)
        result = json.loads(output.getvalue())
        self.assertEqual(result["id"], 9)
        self.assertIn("outcome unknown", result["error"]["message"])

    def test_initialize_supplies_application_instructions(self):
        output = io.StringIO()
        with patch("sys.stdin", io.StringIO('{"id":1,"method":"initialize"}\n')), \
                patch("sys.stdout", output), \
                patch("mcp_stdio.forward", return_value={"id": 1, "result": {}}):
            mcp_stdio.main()
        self.assertIn("missing functional dimensions", json.loads(output.getvalue())["result"]["instructions"])

    def test_python_traceback_is_not_a_success(self):
        page = {"has_more": False, "page": [{"stream": "stderr", "text": "Traceback: invalid shape"}]}
        with patch("probe.forward", return_value={"result": {"content": [{"text": json.dumps(page)}]}}):
            with self.assertRaisesRegex(RuntimeError, "Traceback"):
                probe.execute("bad_shape()")

    def test_pagination_does_not_repeat_execution(self):
        pages = [
            {"has_more": True, "job_token": "job-1", "page_no": 0,
             "page": [{"stream": "stdout", "text": "first"}]},
            {"has_more": False, "page": [{"stream": "stdout", "text": "second"}]},
        ]
        with patch("probe.forward", side_effect=[{"result": {"content": [{"text": json.dumps(p)}]}} for p in pages]) as send:
            self.assertEqual(probe.execute("change()"), "firstsecond")
        self.assertEqual([c.args[0]["params"]["name"] for c in send.call_args_list],
                         ["execute_python", "get_output_page"])


if __name__ == "__main__":
    unittest.main()
