import os
import unittest
from unittest.mock import patch
from test_worker_intake import WorkerIntake

class DiagnosticBridge(WorkerIntake):
    def test_stage_output_is_safe_and_optional(self):
        for enabled in ('0','1'):
            with self.subTest(enabled=enabled), patch.dict(os.environ,CI_INTAKE_DIAGNOSTICS=enabled):
                with self.session('{"private":"SENTINEL_PRIVATE"}') as (listener,proc,_,output):
                    with listener.accept()[0] as conn:
                        conn.makefile('rb').readline();conn.sendall(b'2')
                        out,err=proc.communicate(timeout=4)
                    self.assertEqual(proc.returncode,2)
                    self.assertNotIn(b'SENTINEL_PRIVATE',out+err)
                    if enabled=='1':
                        self.assertIn(b'frame_parsed',err)
                        self.assertIn(b'host_rejected',err)
                    else:self.assertEqual(err,b'')
