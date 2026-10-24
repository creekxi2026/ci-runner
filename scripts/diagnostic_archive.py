"""Typed-only, bounded evidence. Raw Docker output is never written to disk.

The caller owns the foreground lifetime and terminal/release gates. Archive
failure is secondary evidence failure, never permission to release a scope.
"""
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import signal
import stat
import struct
import subprocess
import time

CAP=20*1024*1024
LINE_CAP=2048
STAGES=set('init_config init_credentials init_client init_disk ensure_set auth_refresh auth_registration auth_admin_connection session_create session_refresh session_close bootstrap poll scale acquire ack demand reconcile start diagnostic'.split())
OUTCOMES=set('pending ok empty retry fatal cancelled timeout dropped'.split())
CLASSES=set('none cancelled deadline network_timeout dns tls connection http_unauthorized http_forbidden http_not_found http_rate_limited http_server decode local_config local_io unknown'.split())
COUNTS=set('poll_202 poll_200 poll_401 messages acquired demand started dropped'.split())
IDENTITY=set('window_id source_revision image_digest scope_ref'.split())
FIELDS=IDENTITY|set('schema seq op_id parent_op_id utc stage phase logical_attempt http_attempt duration_ms outcome http_status error_class session_ref counts loss_total'.split())


def parse_time(value):
    # Python 3.9 accepts microseconds, while Go/Docker emit RFC3339Nano.
    # Keep the original text in evidence; truncate only for cursor arithmetic.
    value=re.sub(r'\.(\d+)(?=Z|[+-]\d\d:\d\d$)',lambda m:'.'+(m[1]+'000000')[:6],value)
    return datetime.datetime.fromisoformat(value.replace('Z','+00:00'))


def integer(v,minimum=0):return type(v) is int and minimum<=v<=2**64-1

def validate_identity(value):
    if set(value)!=IDENTITY:raise ValueError('identity keys')
    for k in ('window_id','scope_ref'):
        if not isinstance(value[k],str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,64}',value[k]):raise ValueError('identity value')
    for k,pattern in [('source_revision',r'[a-f0-9]{40}'),('image_digest',r'sha256:[a-f0-9]{64}')]:
        if not isinstance(value[k],str) or not re.fullmatch(pattern,value[k]):raise ValueError('identity value')
    return value


LOCAL_REASONS={
    'cleanup_journal':set('invalid_cleanup_identity cleanup_open_failed unsafe_cleanup_record invalid_cleanup_record_size invalid_cleanup_record foreign_cleanup_record cleanup_update_failed'.split()),
    'scheduler_observation':{'invalid_scheduler_identity','scheduler_identity_changed'},
    'input_assignment':set('invalid_input_runner invalid_input_assignment input_spec_stat_failed input_assignment_read_failed sealed_input_changed input_assignment_write_failed'.split()),
    'completion_cleanup':{'completion_cleanup_pending'},'unknown':{'unknown'},'none':{'none'}}
LOCAL_NUMBERS={'available','started','completed','event_index'}
LOCAL_BOOLS={'name_valid','state_exists','journal_exists','journal_safe','acquired'}
LOCAL_FIELDS={'origin','reason','handler'}|LOCAL_NUMBERS|LOCAL_BOOLS


def validate_local(v):
    if not isinstance(v,dict) or set(v)!=LOCAL_FIELDS:raise ValueError('local keys')
    if not isinstance(v['origin'],str) or not isinstance(v['reason'],str) or v['reason'] not in LOCAL_REASONS.get(v['origin'],set()):raise ValueError('local reason')
    if v['handler'] not in ('started','completed','unknown','none'):raise ValueError('local handler')
    for k in LOCAL_NUMBERS:
        if v[k] is not None and not integer(v[k],1 if k=='event_index' else 0):raise ValueError('local number')
    for k in LOCAL_BOOLS:
        if v[k] is not None and type(v[k]) is not bool:raise ValueError('local boolean')
    if v['event_index'] is not None and v['handler'] not in ('started','completed'):raise ValueError('local event')
    return v


def validate_event(value,identity):
    if set(value) not in (FIELDS,FIELDS|{'local'}) or value['schema']!='ci-stage-v1':raise ValueError('event keys/schema')
    if any(value[k]!=identity[k] for k in IDENTITY):raise ValueError('event identity')
    if value['stage'] not in STAGES or value['phase'] not in ('begin','end','summary') or value['outcome'] not in OUTCOMES or value['error_class'] not in CLASSES:raise ValueError('event enum')
    for k in ('seq','op_id','logical_attempt'):
        if not integer(value[k],1):raise ValueError('event number')
    if not integer(value['loss_total']):raise ValueError('event loss')
    for k in ('parent_op_id','http_attempt','duration_ms'):
        if value[k] is not None and not integer(value[k],1 if k!='duration_ms' else 0):raise ValueError('optional number')
    if value['http_status'] is not None and (type(value['http_status']) is not int or not 100<=value['http_status']<=599):raise ValueError('status')
    if value['session_ref'] is not None and (not isinstance(value['session_ref'],str) or not re.fullmatch(r'[a-f0-9]{64}',value['session_ref'])):raise ValueError('session ref')
    if not isinstance(value['counts'],dict) or set(value['counts'])!=COUNTS or not all(integer(v) for v in value['counts'].values()):raise ValueError('counts')
    if not isinstance(value['utc'],str) or not re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z',value['utc']):raise ValueError('utc')
    parse_time(value['utc'])
    if 'local' in value:
        if value['stage']!='scale' or value['phase']!='end' or value['http_attempt'] is not None:raise ValueError('local placement')
        validate_local(value['local'])
    return value


def private_dir(path):
    p=Path(path);s=p.lstat()
    if not stat.S_ISDIR(s.st_mode) or s.st_uid!=os.getuid() or stat.S_IMODE(s.st_mode)!=0o700:raise ValueError('untrusted directory')
    return p


def exclusive(path,data):
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'wb') as f:f.write(data);f.flush();os.fsync(f.fileno())


def prepare(parent,identity,entropy=os.urandom):
    """Fresh support directory; salt never enters environment, logs or manifests."""
    validate_identity(identity);parent=private_dir(parent)
    p=parent/'diagnostics';p.mkdir(mode=0o700)
    salt=entropy(32)
    if not isinstance(salt,bytes) or len(salt)!=32 or not any(salt):raise ValueError('entropy unavailable')
    exclusive(p/'salt',salt)
    exclusive(p/'profile.json',json.dumps(identity,sort_keys=True).encode())
    return p


def bounded(argv,deadline,cap):
    """Read bounded pipes and kill only this process group on deadline/cap.

    A descendant retaining the pipe cannot hold the foreground indefinitely.
    Log reads return both streams for classification; neither raw stream is
    persisted. Other command stderr is discarded.
    """
    p=subprocess.Popen(argv,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
    sel=selectors.DefaultSelector();sel.register(p.stdout,selectors.EVENT_READ,'out');sel.register(p.stderr,selectors.EVENT_READ,'err')
    out=bytearray();errout=bytearray();total=0
    try:
        while sel.get_map():
            left=deadline-time.monotonic()
            if left<=0:raise TimeoutError('diagnostic read deadline')
            for key,_ in sel.select(min(left,.1)):
                data=os.read(key.fileobj.fileno(),min(65536,cap-total+1))
                if not data:sel.unregister(key.fileobj);continue
                total+=len(data)
                if total>cap:raise ValueError('diagnostic read cap')
                if key.data=='out':out.extend(data)
                else:errout.extend(data)
        left=deadline-time.monotonic()
        if left<=0:raise TimeoutError('diagnostic child deadline')
        if p.wait(timeout=left)!=0:raise ValueError('diagnostic command failed')
        return (bytes(out),bytes(errout)) if argv[:2]==['docker','logs'] else bytes(out)
    finally:
        # Descendant pipes can survive an exited leader; always reap the exact
        # group we created, even after normal completion.
        try:os.killpg(p.pid,signal.SIGKILL)
        except ProcessLookupError:pass
        p.stdout.close();p.stderr.close();sel.close()
        try:p.wait(timeout=.2)
        except subprocess.TimeoutExpired:pass


class Archive:
    def __init__(self,parent,identity,container,*,read=bounded,clock=time.monotonic,cap=CAP):
        validate_identity(identity)
        if not isinstance(container,str) or not re.fullmatch('[a-f0-9]{64}',container):raise ValueError('exact container ID required')
        self.parent=private_dir(parent);self.directory=self.parent/'archive';self.directory.mkdir(mode=0o700)
        self.identity=identity.copy();self.container=container;self.read=read;self.clock=clock;self.cap=min(CAP,cap)
        if self.cap<4096:raise ValueError('archive cap too small')
        self.bytes=0;self.input_bytes=0;self.last=0;self.seen={};self.cursor=None;self.initial=None;self.final=None
        self.rejected=0;self.duplicates=0;self.gaps=0;self.loss=0;self.failures=[];self.truncated=False;self.finished=False;self.shutdown_summary=False;self.pending=set()
        self.local_reasons={};self.local_observations=0;self.local_failures=0
        self.events=os.fdopen(os.open(self.directory/'events.ndjson',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600),'wb')

    def identity_read(self,deadline):
        b=self.read(['docker','inspect',self.container],deadline,256*1024)
        rows=json.loads(b);assert len(rows)==1
        c=rows[0]
        assert c['Id']==self.container and c['Image']==self.identity['image_digest']
        result={k:c[k] for k in ('Id','Image','RestartCount')}
        result['State']={k:c['State'][k] for k in ('Running','Status','ExitCode','StartedAt','FinishedAt','OOMKilled')}
        if type(result['RestartCount']) is not int or result['RestartCount']<0:raise ValueError('invalid restart count')
        state=result['State']
        if type(state['Running']) is not bool or type(state['OOMKilled']) is not bool or type(state['ExitCode']) is not int or state['Status'] not in ('created','restarting','running','removing','paused','exited','dead'):raise ValueError('invalid state')
        for key in ('StartedAt','FinishedAt'):
            if not isinstance(state[key],str) or not re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z',state[key]):raise ValueError('invalid identity time')
            parse_time(state[key])
        if self.initial:
            assert result['State']['StartedAt']==self.initial['State']['StartedAt'] and result['RestartCount']==self.initial['RestartCount'],'container identity drift'
        return result

    def ingest(self,raw,framed=False,stream_name="stdout"):
        self.input_bytes+=len(raw)
        if self.input_bytes>self.cap:raise ValueError('archive input cap')
        streams=[]
        if framed:
            i=0
            while i<len(raw):
                if len(raw)-i<8:raise ValueError('truncated Docker header')
                stream=raw[i];n=struct.unpack('>I',raw[i+4:i+8])[0]
                if stream not in (1,2) or raw[i+1:i+4]!=b'\0\0\0' or n>self.cap or i+8+n>len(raw):raise ValueError('Docker framing')
                streams.append(('stdout' if stream==1 else 'stderr',raw[i+8:i+8+n]));i+=8+n
        else:streams=[(stream_name,raw)]
        # Docker frame boundaries can divide a line. Keep separate buffers and
        # completion order, bounded by the already enforced total input cap.
        buffers={'stdout':bytearray(),'stderr':bytearray()}
        for stream,data in streams:
            for part in data.splitlines(keepends=True):
                buffers[stream].extend(part)
                if not part.endswith(b'\n'):continue
                line=bytes(buffers[stream]);buffers[stream].clear()
                if not self.typed_candidate(line,stream):continue
                self.line(line,stream)
        for stream,data in buffers.items():
            if data and self.typed_candidate(bytes(data),stream):self.truncated=True

    @staticmethod
    def typed_candidate(line,stream):
        # The controller reserves stdout for typed records. Plain stderr is
        # the existing business logger and must never enter the archive.
        # JSON-like stderr remains strict, including malformed typed JSON.
        if stream!='stderr':return True
        payload=line.split(b' ',1)[-1].lstrip()
        return payload.startswith((b'{',b'[')) or b'ci-stage-v1' in payload

    def line(self,line,stream):
        try:
            # Docker --timestamps; program utc is separately retained in event.
            stamp,event=line.split(b' ',1);stamp=stamp.decode('ascii')
            parse_time(stamp)
            if len(event)>LINE_CAP:raise ValueError('event cap')
            value=validate_event(json.loads(event.decode('utf-8')),self.identity)
            digest=hashlib.sha256(event).hexdigest();seq=value['seq']
            if seq in self.seen:
                if self.seen[seq]!=digest:raise ValueError('sequence collision')
                self.duplicates+=1;return
            if seq<=self.last:raise ValueError('sequence regression')
            if seq!=self.last+1:self.gaps+=1
            # Bound deduplication memory along with archive size.
            row=json.dumps(dict(docker_utc=stamp,stream=stream,event=value),sort_keys=True,separators=(',',':')).encode()+b'\n'
            if self.bytes+len(row)>self.cap-4096:raise OverflowError('archive output cap')
            self.events.write(row);self.bytes+=len(row);self.seen[seq]=digest;self.last=seq;self.loss=max(self.loss,value['loss_total']);self.cursor=stamp
            key=(value['op_id'],value['http_attempt'])
            if value['phase']=='begin':self.pending.add(key)
            elif value['phase']=='end':self.pending.discard(key)
            if value['stage']=='diagnostic' and value['phase']=='summary':self.shutdown_summary=True
            if 'local' in value:
                local=value['local'];self.local_observations+=1
                if value['outcome']!='ok':
                    self.local_failures+=1
                    reason=local['reason'];self.local_reasons[reason]=self.local_reasons.get(reason,0)+1
        except OverflowError:raise
        except (ValueError,TypeError,KeyError,AssertionError,UnicodeError):self.rejected+=1

    def sample(self,deadline):
        if self.finished:raise ValueError('archive finalized')
        try:
            if self.clock()>=deadline:raise TimeoutError('diagnostic budget exhausted')
            identity=self.identity_read(min(deadline,self.clock()+2))
            if self.initial is None:self.initial=identity
            argv=['docker','logs','--timestamps']
            if self.cursor:
                cursor=parse_time(self.cursor)-datetime.timedelta(seconds=1)
                argv+=['--since',cursor.isoformat()]
            argv.append(self.container)
            left=self.cap-self.input_bytes
            if left<=0:raise OverflowError('archive input exhausted')
            raw=self.read(argv,min(deadline,self.clock()+2),left)
            if isinstance(raw,tuple):
                self.ingest(raw[0]);self.ingest(raw[1],stream_name="stderr")
            else:self.ingest(raw)
            # Known corruption is irreversible; report it to the foreground
            # cancellation gate now. A transient empty running stream has only
            # the caller's existing window budget, never a new grace budget.
            healthy=not any((self.failures,self.truncated,self.rejected,self.gaps,self.loss))
            if not identity['State']['Running']:
                healthy=healthy and self.last>0 and self.shutdown_summary and not self.pending
            return healthy
        except (Exception,KeyboardInterrupt) as exc:
            self.failures.append('timeout' if isinstance(exc,TimeoutError) else 'read_failed');self.truncated=True;return False

    def finish(self,deadline):
        if self.finished:raise ValueError('duplicate final archive')
        try:
            self.sample(deadline)
            self.final=self.identity_read(min(deadline,self.clock()+2))
        except (Exception,KeyboardInterrupt):self.failures.append('final_identity_unknown')
        self.finished=True
        try:self.events.flush();os.fsync(self.events.fileno())
        finally:self.events.close()
        complete=bool(self.initial and self.final and not any((self.failures,self.truncated,self.rejected,self.gaps,self.loss)))
        # A shutdown summary proves orderly sink completion; otherwise incomplete.
        # No collector-generated end substitutes for an absent business event.
        complete=complete and self.last>0 and self.shutdown_summary and not self.pending and not self.final['State']['Running']
        manifest=dict(schema='ci-diagnostic-archive-v1',identity=self.identity,container_id=self.container,
            initial=self.initial,final=self.final,last_seq=self.last,input_bytes=self.input_bytes,event_bytes=self.bytes,
            rejected=self.rejected,duplicates=self.duplicates,gaps=self.gaps,loss_total=self.loss,
            truncated=self.truncated,incomplete=not complete,failures=self.failures,cap=self.cap,
            shutdown_summary=self.shutdown_summary,pending_operations=len(self.pending))
        if self.local_observations:
            manifest['local_scale']=dict(observations=self.local_observations,failures=self.local_failures,reasons=self.local_reasons)
        exclusive(self.directory/'manifest.json',json.dumps(manifest,sort_keys=True).encode())
        hashes={p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in self.directory.iterdir()}
        exclusive(self.directory/'SHA256.json',json.dumps(hashes,sort_keys=True).encode())
        # Salt remains available until after the stopped observer's final read.
        salt=self.parent/'salt'
        if salt.exists():
            st=salt.lstat()
            if stat.S_ISREG(st.st_mode) and st.st_uid==os.getuid() and stat.S_IMODE(st.st_mode)==0o600 and st.st_nlink==1:salt.unlink()
            else:raise ValueError('salt ownership drift')
        return manifest
