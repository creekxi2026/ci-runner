import importlib.util
from pathlib import Path
import shutil
import tempfile
import unittest
ROOT=Path(__file__).resolve().parents[1]
spec=importlib.util.spec_from_file_location('diagnostic_preflight',ROOT/'scripts/image_preflight.py')
p=importlib.util.module_from_spec(spec);spec.loader.exec_module(p)
class LocalModule(unittest.TestCase):
    def test_exact_patch_closure_and_license(self):self.assertGreater(p.verify_local_module(ROOT),30)
    def test_drift_missing_copy_and_unrecorded_file_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            shutil.copytree(ROOT/'third_party',root/'third_party')
            shutil.copyfile(ROOT/'Dockerfile',root/'Dockerfile');shutil.copyfile(ROOT/'go.mod',root/'go.mod')
            for name in ('diagnostic.go','LICENSE'):
                file=root/'third_party/scaleset'/name;original=file.read_bytes();file.write_bytes(original+b'\n')
                with self.assertRaises(ValueError):p.verify_local_module(root)
                file.write_bytes(original)
            file=root/'Dockerfile';file.write_text(file.read_text().replace('COPY third_party/scaleset ./third_party/scaleset',''))
            with self.assertRaises(ValueError):p.verify_local_module(root)
if __name__=='__main__':unittest.main()
