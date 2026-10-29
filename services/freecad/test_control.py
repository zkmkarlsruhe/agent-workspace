# SPDX-License-Identifier: MIT

import json
import io
from types import SimpleNamespace
from unittest.mock import Mock
import os
import unittest
from unittest.mock import patch
os.environ['WORKSPACE_FREECAD_SESSION'] = 'a' * 32
import control


class ControlTests(unittest.TestCase):
    def setUp(self):
        control._requests.clear()
        self.args = dict(session_id='a'*32, document='Part', revision='current',
                         code='doc.addObject("Part::Box", "Box")', request_id='request_123')

    def test_control_auth_and_manager_source(self):
        for peer, token, status in [("10.90.1.10", "Bearer secret", 403),
                                    ("10.90.1.3", "", 401),
                                    ("10.90.1.3", "Bearer wrong", 401),
                                    ("10.90.1.3", "Bearer ü", 401),
                                    ("10.90.1.3", "Bearer secret", 200)]:
            handler = control.Handler.__new__(control.Handler)
            body = b'{"jsonrpc":"2.0","id":1,"method":"ping"}'
            handler.server = SimpleNamespace(manager_address="10.90.1.3")
            handler.client_address = (peer, 1234)
            handler.connection = Mock()
            handler.headers = {"Authorization": token, "Content-Length": str(len(body))}
            handler.path = "/mcp"
            handler.rfile, handler.wfile = io.BytesIO(body), io.BytesIO()
            handler.send_error, handler.send_response = Mock(), Mock()
            handler.send_header, handler.end_headers = Mock(), Mock()
            with self.subTest(peer=peer, token=token), patch.object(control.Path, "read_text", return_value="secret"):
                handler.do_POST()
                (handler.send_response if status == 200 else handler.send_error).assert_called_once_with(status)
                if status != 200:
                    self.assertEqual(handler.rfile.tell(), 0)

    def test_validation(self):
        for name, changes in [('foreign session', {'session_id':'b'*32}),
                              ('unknown key', {'path':'/other'}), ('code type', {'code':False}),
                              ('long code', {'code':'x'*32001}), ('request id', {'request_id':'../../xx'})]:
            with self.subTest(name=name), self.assertRaises(ValueError):
                control.validate('freecad_execute', self.args | changes)
        with self.assertRaises(ValueError):
            control.validate('freecad_output', dict(session_id='a'*32, job_token='abc', page_no=True))

    def test_repeated_request_executes_once(self):
        page={'page':[], 'has_more':False}
        with patch.object(control, 'forward', return_value={'result':{'content':[{'type':'text','text':json.dumps(page)}]}}) as forward:
            first=control.call('freecad_execute', self.args)
            self.assertEqual(first, control.call('freecad_execute', self.args))
            self.assertEqual(1, forward.call_count)
            with self.assertRaises(ValueError):
                control.call('freecad_execute', self.args | {'code':'different'})

    def test_uncertain_write_never_repeated(self):
        with patch.object(control, 'forward', side_effect=TimeoutError) as forward:
            with self.assertRaises(TimeoutError): control.call('freecad_execute', self.args)
            result=control.call('freecad_execute', self.args)
            self.assertTrue(result['isError'])
            self.assertEqual('outcome_unknown', result['structuredContent']['status'])
            self.assertEqual(1, forward.call_count)

    def test_stderr_marks_failure(self):
        page={'page':[{'stream':'stderr', 'text':'conflict'}], 'has_more':False}
        with patch.object(control, 'forward', return_value={'result':{'content':[{'type':'text','text':json.dumps(page)}]}}):
            self.assertTrue(control.call('freecad_execute', self.args)['isError'])


if __name__=='__main__': unittest.main()
