import pathlib,tempfile,subprocess,unittest,os
ROOT=pathlib.Path(__file__).resolve().parents[1]
class ImageToolcache(unittest.TestCase):
 def test_seed_readonly_image_tools_without_copy_or_download(self):
  script=ROOT/'toolcache-init.sh'
  self.assertTrue(script.exists(),'image toolcache seeding is missing')
  temp=ROOT/'.operations'/'test-temp';temp.mkdir(parents=True,exist_ok=True)
  with tempfile.TemporaryDirectory(dir=temp) as d:
   p=pathlib.Path(d);cache=p/'cache';go=p/'image-go';node=p/'image-node';go.mkdir();node.mkdir()
   for _ in range(2):subprocess.run(['sh',str(script),str(cache),str(go),str(node)],check=True)
   for name,version,target in [('go','1.26.9',go),('node','24.14.0',node)]:
    entry=cache/name/version/'arm64'
    self.assertTrue(entry.is_symlink());self.assertEqual(entry.resolve(),target)
    self.assertTrue(entry.with_name('arm64.complete').is_file())
