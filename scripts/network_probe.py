"""Fail if any forbidden direct connection or proxy destination succeeds."""
import os, socket, urllib.request, urllib.error
hosts=['host.docker.internal','host.orb.internal','gateway.docker.internal','192.168.1.1','10.0.0.1','169.254.169.254','::1','fc00::1','fe80::1','::ffff:192.168.1.1','1.1.1.1']
# Peer fixture is supplied by controller; address is not a credential.
if os.environ.get('CI_PEER_PROBE_IP'): hosts.append(os.environ['CI_PEER_PROBE_IP'])
for host in hosts:
 for port in [80,443,2375,7897]:
  try:
   s=socket.create_connection((host,port),timeout=1);s.close()
  except (OSError, socket.gaierror):print('DIRECT BLOCKED',host,port)
  else:raise SystemExit('DIRECT BYPASS '+host+':'+str(port))
proxy=urllib.request.ProxyHandler({'http':os.environ['http_proxy'],'https':os.environ['https_proxy']})
opener=urllib.request.build_opener(proxy)
for host in ['127.0.0.1','192.168.1.1','169.254.169.254','[::1]','host.docker.internal','host.orb.internal']:
 try:
  with opener.open('http://'+host+'/',timeout=5) as r:raise SystemExit('PROXY BYPASS '+host)
 except urllib.error.HTTPError as e:
  assert e.code==403,(host,e.code);print('PROXY DENIED',host)
 except urllib.error.URLError as e:
  # An unresolved special hostname isn't proof of policy; literals above are.
  print('PROXY CONNECTION BLOCKED',host,str(e.reason))
print('network isolation probes passed')
