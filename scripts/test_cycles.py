import unittest, tempfile, pathlib, importlib.util, types, os
from unittest.mock import patch
spec=importlib.util.spec_from_file_location('importer',pathlib.Path(__file__).with_name('import-review-config.py'));m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
class Cycles(unittest.TestCase):
 def setup_args(self):
  t=tempfile.TemporaryDirectory();self.addCleanup(t.cleanup);root=pathlib.Path(t.name);cfg=root/'input.yaml';cfg.write_text("provider:\n  minimax:\n    npm: '@ai-sdk/anthropic'\n    options:\n      baseURL: 'https://api.minimax.cn/anthropic'\n      apiKey: 'fixture-api-token'\ndefaultModel: 'minimax/MiniMax-M3.1-Flash'\n")
  return types.SimpleNamespace(minimax_mcp_config=None,minimax_config=str(cfg),jev_model='jev-latest',jev_proxy_url='http://127.0.0.1:7890',output_dir=str(root/'out'))
 def test_cycle18_dangling_output_link_is_rejected(self):
  a=self.setup_args();p=pathlib.Path(a.output_dir);target=p.parent/'unexpected';p.symlink_to(target,target_is_directory=True)
  with patch.dict(os.environ,{'TYPESAFE_API_KEY':'fixture-jev-token'}):
   with self.assertRaises(m.ImportErrorSafe):m.import_config(a)
  self.assertFalse(target.exists())
 def test_cycle19_replaced_output_directory_cannot_redirect_keys(self):
  a=self.setup_args();out=pathlib.Path(a.output_dir);moved=out.parent/'held';attacker=out.parent/'attacker';attacker.mkdir();original=m.write_private_json;swapped=False
  def redirect(path,value,*args,**kwargs):
   nonlocal swapped
   if not swapped:
    swapped=True;out.rename(moved);out.symlink_to(attacker,target_is_directory=True)
   return original(path,value,*args,**kwargs)
  with patch.dict(os.environ,{'TYPESAFE_API_KEY':'fixture-jev-token'}),patch.object(m,'write_private_json',redirect):
   try:m.import_config(a)
   except m.ImportErrorSafe:pass
  self.assertEqual(list(attacker.iterdir()),[], 'credentials followed replaced directory')
 def test_cycle20_proxy_port_rejected_before_writing(self):
  for port in ['abc','99999','0']:
   a=self.setup_args();a.jev_proxy_url='http://127.0.0.1:'+port
   with patch.dict(os.environ,{'TYPESAFE_API_KEY':'fixture-jev-token'}):
    with self.assertRaises(m.ImportErrorSafe):m.import_config(a)
   self.assertFalse(pathlib.Path(a.output_dir).exists())
