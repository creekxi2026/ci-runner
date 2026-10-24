import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest

ROOT=Path(__file__).resolve().parents[1]
spec=importlib.util.spec_from_file_location('local_archive',ROOT/'scripts/diagnostic_archive.py')
a=importlib.util.module_from_spec(spec);spec.loader.exec_module(a)
STAMP='2026-10-25T00:00:00Z'
ID=dict(window_id='local-scale',source_revision='a'*40,image_digest='sha256:'+'b'*64,scope_ref='fixture')
CID='c'*64

def local():return dict(origin='unknown',reason='unknown',handler='unknown',**{k:None for k in a.LOCAL_NUMBERS|a.LOCAL_BOOLS})
def event():return dict(schema='ci-stage-v1',**ID,seq=1,op_id=1,parent_op_id=None,utc=STAMP,stage='scale',phase='end',logical_attempt=1,http_attempt=None,duration_ms=0,outcome='fatal',http_status=None,error_class='unknown',session_ref=None,counts={k:0 for k in a.COUNTS},loss_total=0,local=local())

class LocalArchive(unittest.TestCase):
    def test_rfc3339_fraction_widths(self):
        for width in range(1,10):
            v=event();v["utc"]="2026-10-25T00:00:00."+"1"*width+"Z"
            self.assertEqual(a.validate_event(v,ID)["utc"],v["utc"])

    def test_closed_fields_and_pairs(self):
        a.validate_event(event(),ID)
        for origin,reasons in a.LOCAL_REASONS.items():
            for reason in reasons:a.validate_local(dict(local(),origin=origin,reason=reason,handler='started',event_index=1))
        invalid=[]
        for k in ('origin','reason','handler','name_valid','event_index'):
            v=event();v['local'][k]='SECRET';invalid.append(v)
        for v in (None,{},dict(local(),extra='SECRET'),dict(local(),started=True),dict(local(),acquired=0),dict(local(),event_index=0),dict(local(),origin='cleanup_journal'),dict(local(),handler='none',event_index=1)):
            invalid.append(dict(event(),local=v))
        invalid.extend([dict(event(),stage='poll'),dict(event(),phase='begin'),dict(event(),http_attempt=1),dict(event(),extra='SECRET')])
        for v in invalid:
            with self.subTest(v=v),self.assertRaises((ValueError,TypeError)):a.validate_event(v,ID)
        old=event();old.pop('local');self.assertEqual(a.validate_event(old,ID),old)

    def test_real_bounded_sample_and_projection(self):
        inputs=os.environ.get('R33_EVENT_OUTPUT')
        if not inputs:self.skipTest('Go event artifacts supplied by layered verification')
        paths=sorted(Path(inputs).glob('*.ndjson'));self.assertGreaterEqual(len(paths),18)
        output=Path(os.environ.get('R33_ARCHIVE_OUTPUT',inputs))
        results=[]
        for path in paths:
            records=[json.loads(x) for x in path.read_bytes().splitlines()]
            self.assertTrue(any('local' in e for e in records));self.assertTrue(all(len(x)+1<=2048 for x in path.read_bytes().splitlines()))
            with tempfile.TemporaryDirectory(prefix='r33-archive-') as tmp:
                root=Path(tmp);root.chmod(0o700);(root/'bin').mkdir()
                (root/'raw').write_bytes(b''.join(STAMP.encode()+b' '+json.dumps(e,separators=(',',':')).encode()+b'\n' for e in records))
                state=[dict(Id=CID,Image=ID['image_digest'],RestartCount=0,State=dict(Running=False,Status='exited',ExitCode=0,StartedAt=STAMP,FinishedAt=STAMP,OOMKilled=False))]
                (root/'inspect').write_text(json.dumps(state))
                # Actual subprocess pipes and foreground bounded reader, with a
                # finite Docker executable fixture; never a live socket/client.
                command=root/'bin/docker';command.write_text('#!'+sys.executable+'\nfrom pathlib import Path\nimport sys\np=Path(__file__).resolve().parents[1]\nif sys.argv[1]=="inspect":sys.stdout.buffer.write((p/"inspect").read_bytes())\nelif sys.argv[1]=="logs":\n sys.stdout.buffer.write((p/"raw").read_bytes());sys.stderr.write("SECRET-business-stderr\\n")\nelse:raise SystemExit(8)\n');command.chmod(0o700)
                old=os.environ['PATH'];os.environ['PATH']=str(root/'bin')+os.pathsep+old
                try:
                    arc=a.Archive(root,ID,CID)
                    try:
                        healthy=arc.sample(time.monotonic()+3)
                    finally:manifest=arc.finish(time.monotonic()+3)
                    self.assertTrue(healthy,manifest)
                finally:os.environ['PATH']=old
                self.assertFalse(manifest['incomplete']);self.assertEqual(manifest['rejected'],0)
                captured=[json.loads(x)['event'] for x in (root/'archive/events.ndjson').read_bytes().splitlines()]
                self.assertEqual(captured,records)
                locals_=[r['local'] for r in records if 'local' in r]
                self.assertEqual(manifest['local_scale']['observations'],len(locals_))
                self.assertEqual(manifest['local_scale']['failures'],sum(r['outcome']!='ok' for r in records if 'local' in r))
                self.assertNotIn(b'SECRET',b''.join(p.read_bytes() for p in (root/'archive').iterdir()))
                results.append(dict(case=path.stem,local_scale=manifest['local_scale'],events=len(captured),incomplete=False))
        output.mkdir(parents=True,exist_ok=True);(output/'archive-results.json').write_text(json.dumps(results,indent=2))

if __name__=='__main__':unittest.main()
