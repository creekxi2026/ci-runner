"""Catch bypassing the one-shot bootstrap before the actual CLI packet path."""
import hmac,json,os,pathlib,socket,struct,subprocess,sys,tempfile,threading,unittest
ROOT=pathlib.Path(__file__).resolve().parents[1]
class IntakeBinding(unittest.TestCase):
 def test_cli_refuses_unbound_fds_before_connecting(self):
  source_r,source_w=os.pipe();output_r,output_w=os.pipe()
  try:
   env=dict(os.environ);env.pop('CI_INTAKE_BOOTSTRAP_FD',None)
   p=subprocess.Popen([sys.executable,str(ROOT/'worker_intake.py'),'spawnclient',str(source_r),str(output_w)],pass_fds=(source_r,output_w),env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
   data=json.dumps({}).encode('utf-16-le');os.write(source_w,struct.pack('<ii',1,len(data))+data)
   out,err=p.communicate(timeout=3)
   self.assertEqual(p.returncode,2);self.assertEqual(out+err,b'')
  finally:
   for fd in (source_r,source_w,output_r,output_w):
     if fd is not None:os.close(fd)
 def test_cli_consumes_inherited_socket_capability_before_packet(self):
  # The fixed production socket is isolated inside this disposable container.
  if os.geteuid()!=0 or not sys.platform.startswith('linux'):self.skipTest('isolated Linux socket installation')
  root=pathlib.Path('/run/ci-intake');root.mkdir(exist_ok=True)
  address=root/'session.sock'
  source_r,source_w=os.pipe();output_r,output_w=os.pipe();broker,client=socket.socketpair()
  secret=os.urandom(32);results=[]
  with socket.socket(socket.AF_UNIX) as listener:
   listener.bind(str(address));listener.listen(1);listener.settimeout(3)
   def deliver():
    broker.settimeout(3)
    try:
     request=broker.recv(1);results.append(request==b'1');broker.sendall(secret)
    except OSError:results.append(False)
    finally:broker.close()
   thread=threading.Thread(target=deliver);thread.start()
   p=subprocess.Popen([sys.executable,str(ROOT/'worker_intake.py'),'spawnclient',str(source_r),str(output_w)],env=dict(os.environ,CI_INTAKE_BOOTSTRAP_FD=str(client.fileno())),pass_fds=(source_r,output_w,client.fileno()),stdout=subprocess.PIPE,stderr=subprocess.PIPE)
   client.close()
   try:
    data=json.dumps({'requestId':131}).encode('utf-16-le');os.write(source_w,struct.pack('<ii',1,len(data))+data)
    with listener.accept()[0] as conn:
     conn.settimeout(3);auth=bytearray()
     while len(auth)<32:auth.extend(conn.recv(32-len(auth)))
     self.assertTrue(hmac.compare_digest(bytes(auth),secret),'CLI sent packet before allocation binding')
     conn.sendall(b'1')
     packet=conn.makefile('rb').readline();self.assertEqual(json.loads(packet)['schema'],'ci-job-intake/v1')
     conn.sendall(b'2')
    out,err=p.communicate(timeout=4)
    self.assertEqual(p.returncode,2);self.assertEqual(out+err,b'');self.assertEqual(results,[True])
   finally:
    if p.poll() is None:p.kill()
    p.communicate();thread.join(4)
    for fd in (source_r,source_w,output_r,output_w):
     if fd is not None:os.close(fd)
    address.unlink(missing_ok=True)
 def test_cancel_interrupts_controller_binding_ack_wait(self):
  if os.geteuid()!=0 or not sys.platform.startswith('linux'):self.skipTest('isolated Linux socket installation')
  root=pathlib.Path('/run/ci-intake');root.mkdir(exist_ok=True);address=root/'session.sock'
  source_r,source_w=os.pipe();output_r,output_w=os.pipe();broker,client=socket.socketpair()
  p=None
  with socket.socket(socket.AF_UNIX) as listener:
   listener.bind(str(address));listener.listen(1);listener.settimeout(3)
   try:
    p=subprocess.Popen([sys.executable,str(ROOT/'worker_intake.py'),'spawnclient',str(source_r),str(output_w)],env=dict(os.environ,CI_INTAKE_BOOTSTRAP_FD=str(client.fileno())),pass_fds=(source_r,output_w,client.fileno()),stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    client.close();os.close(output_w);output_w=None;broker.settimeout(3);self.assertEqual(broker.recv(1),b'1');broker.sendall(os.urandom(32));broker.close()
    data=json.dumps({}).encode('utf-16-le');os.write(source_w,struct.pack('<ii',1,len(data))+data)
    with listener.accept()[0] as conn:
     conn.settimeout(3);received=bytearray()
     while len(received)<32:received.extend(conn.recv(32-len(received)))
     received[:]=bytes(32)
     os.write(source_w,struct.pack('<ii',2,0))
     try:out,err=p.communicate(timeout=1)
     except subprocess.TimeoutExpired:self.fail('cancellation retained binding acknowledgement wait')
     self.assertEqual(p.returncode,2);self.assertEqual(out+err,b'');self.assertEqual(os.read(output_r,1),b'')
   finally:
    if p is not None:
     if p.poll() is None:p.kill()
     p.communicate()
    broker.close();client.close()
    for fd in (source_r,source_w,output_r,output_w):
     if fd is not None:os.close(fd)
    address.unlink(missing_ok=True)
