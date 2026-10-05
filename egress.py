"""Public-only HTTP/CONNECT proxy. Resolve once, validate all, dial pinned IP."""
import ipaddress, socket, socketserver, select, urllib.parse, urllib.request, json, os, time, re, sys

# A quiet TLS peer may legitimately wait on server work. Still finite; the job's
# independent 3600s hard lifetime bounds the helper container as a whole.
RELAY_IDLE_SECONDS = 300


class NoResolverRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        return None


def resolve(host, port):
    endpoint = os.environ.get('PUBLIC_EGRESS_DOH_URL', '')
    if not endpoint:
        return socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    u = urllib.parse.urlsplit(endpoint)
    if u.scheme != 'https' or not u.hostname or u.username or u.password or u.query or u.fragment or u.port not in (None, 443):
        raise ValueError('invalid HTTPS DNS resolver')
    try:
        literal = ipaddress.ip_address(host)
    except ValueError:
        literal = None
    if literal is not None:
        family = socket.AF_INET if literal.version == 4 else socket.AF_INET6
        address = (str(literal), port) if literal.version == 4 else (str(literal), port, 0, 0)
        return [(family, socket.SOCK_STREAM, socket.IPPROTO_TCP, '', address)]
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoResolverRedirect())
    answers = []
    for kind, family in ((1, socket.AF_INET), (28, socket.AF_INET6)):
        query = urllib.parse.urlencode({'name': host, 'type': kind})
        request = urllib.request.Request(endpoint + '?' + query, headers={'Accept': 'application/dns-json'})
        with opener.open(request, timeout=10) as response:
            body = response.read(65537)
        if len(body) > 65536:
            raise ValueError('DNS response too large')
        data = json.loads(body)
        if not isinstance(data, dict) or data.get('Status') != 0 or not isinstance(data.get('Answer', []), list):
            raise OSError('HTTPS DNS lookup failed')
        for answer in data.get('Answer', []):
            if not isinstance(answer, dict):
                raise ValueError('invalid DNS answer')
            if answer.get('type') != kind:
                continue
            ip = ipaddress.ip_address(answer.get('data', ''))
            if ip.version != (4 if kind == 1 else 6):
                raise ValueError('invalid DNS address family')
            address = (str(ip), port) if kind == 1 else (str(ip), port, 0, 0)
            answers.append((family, socket.SOCK_STREAM, socket.IPPROTO_TCP, '', address))
    return answers

def upstream_tunnel(address, value):
    u = urllib.parse.urlsplit(value)
    if u.scheme != 'http' or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('', '/'):
        raise ValueError('invalid trusted upstream')
    deadline = time.monotonic() + 15
    s = socket.create_connection((u.hostname, u.port or 80), timeout=15)
    try:
        def remaining_timeout():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError('upstream handshake deadline exceeded')
            s.settimeout(remaining)
        remaining_timeout()
        host = '[' + address[0] + ']' if ':' in address[0] else address[0]
        authority = host + ':' + str(address[1])
        s.sendall(('CONNECT ' + authority + ' HTTP/1.1\r\nHost: ' + authority + '\r\n\r\n').encode('ascii'))
        header = b''
        while not header.endswith(b'\r\n\r\n'):
            remaining_timeout()
            b = s.recv(1)
            if not b or len(header) >= 32768:
                raise OSError('invalid upstream response')
            header += b
        remaining_timeout()
        status = header.split(b'\r\n', 1)[0].split()
        if len(status) < 2 or status[0] not in (b'HTTP/1.0', b'HTTP/1.1') or status[1] != b'200':
            raise OSError('upstream refused connection')
        s.settimeout(15)
        return s
    except Exception:
        s.close()
        raise


def public(value):
    ip = ipaddress.ip_address(value)
    if getattr(ip, 'ipv4_mapped', None):
        ip = ip.ipv4_mapped
    if ip.version == 6 and any(ip in ipaddress.ip_network(n) for n in ('2002::/16','2001::/32','64:ff9b::/96','64:ff9b:1::/48')):
        return False
    return ip.is_global and not ip.is_multicast and not ip.is_reserved

def connect(host, port, diagnostic=None):
    diagnostic = diagnostic if diagnostic is not None else {}
    diagnostic['phase'] = 'policy'
    if port not in (80, 443):
        raise ValueError('port denied')
    diagnostic['phase'] = 'resolve'
    answers = resolve(host, port)
    if not answers:
        raise OSError('DNS returned no addresses')
    diagnostic['phase'] = 'policy'
    if any(not public(a[4][0]) for a in answers):
        raise ValueError('destination denied')
    upstream = os.environ.get('PUBLIC_EGRESS_UPSTREAM_PROXY', '')
    last_error = None
    for family, kind, proto, _, address in answers:
        diagnostic.update(phase='upstream_handshake' if upstream else 'dial', peer_ip=str(ipaddress.ip_address(address[0])))
        if upstream:
            try:
                return upstream_tunnel(address, upstream)
            except OSError as error:
                last_error = error
                continue
        s = socket.socket(family, kind, proto); s.settimeout(15)
        try:
            s.connect(address); return s
        except OSError as error:
            last_error = error
            s.close()
    raise last_error or OSError('connection failed')

class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        remote = None
        response_started = False
        started = time.monotonic()
        last_io = started
        diagnostic = dict(event='egress_close', hostname=None, peer_ip=None,
                          port=None, phase='request', error_type=None,
                          close_reason='eof', close_direction='both',
                          bytes_client_to_server=0, bytes_server_to_client=0,
                          eof_directions=[])
        try:
            self.connection.settimeout(20)
            first = self.rfile.readline(8193)
            if len(first) > 8192: raise ValueError('request too long')
            method, target, version = first.decode('ascii').strip().split(' ')
            headers=[]; size=0
            while True:
                line=self.rfile.readline(8193); size+=len(line)
                if size>32768 or not line: raise ValueError('headers')
                if line==b'\r\n': break
                headers.append(line)
            if method=='CONNECT':
                u=urllib.parse.urlsplit('//'+target)
                port = u.port or 443
            else:
                u=urllib.parse.urlsplit(target)
                if u.scheme!='http' or u.username or u.password: raise ValueError('scheme')
                port = u.port or 80
            # Only DNS-label syntax, not raw authority/URL, enters diagnostics.
            host = u.hostname
            labels = (host[:-1] if host and host.endswith('.') else host or '').split('.')
            if host and len(host) <= 253 and all(re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?', label) for label in labels):
                diagnostic['hostname'] = host
            diagnostic.update(port=port, phase='policy')
            remote=connect(host, port, diagnostic=diagnostic)
            diagnostic['phase'] = 'response'
            if method=='CONNECT':
                response_started = True
                self.wfile.write(b'HTTP/1.1 200 Connection Established\r\n\r\n');self.wfile.flush()
            else:
                path=u.path or '/'
                if u.query: path+='?'+u.query
                remote.sendall((method+' '+path+' '+version+'\r\n').encode('ascii'))
                remote.sendall(b''.join(h for h in headers if not h.lower().startswith((b'proxy-',b'connection:')))+b'Connection: close\r\n\r\n')
            # HTTP downloads and CONNECT only; POST bodies are relayed from the
            # unbuffered input stream so no buffered bytes get lost.
            readers = [self.connection, remote]
            self.connection.settimeout(RELAY_IDLE_SECONDS)
            remote.settimeout(RELAY_IDLE_SECONDS)
            diagnostic['phase'] = 'relay'
            last_io = time.monotonic()
            while readers:
                diagnostic['close_direction'] = 'both'
                ready,_,_=select.select(readers,[],[],RELAY_IDLE_SECONDS)
                if not ready:
                    diagnostic.update(close_reason='idle_timeout', error_type='TimeoutError')
                    break
                for src in ready:
                    direction = 'client' if src is self.connection else 'server'
                    diagnostic['close_direction'] = direction
                    data=src.recv(65536)
                    if not data:
                        readers.remove(src)
                        diagnostic['eof_directions'].append(direction)
                        (remote if src is self.connection else self.connection).shutdown(socket.SHUT_WR)
                        continue
                    if src is remote: response_started = True
                    (remote if src is self.connection else self.connection).sendall(data)
                    diagnostic['bytes_client_to_server' if src is self.connection else 'bytes_server_to_client'] += len(data)
                    last_io = time.monotonic()
            if not readers: diagnostic['close_direction'] = 'both'
        except (OSError,ValueError,UnicodeError) as error:
            diagnostic['error_type'] = type(error).__name__
            diagnostic['close_reason'] = 'error'
            if response_started: return
            if isinstance(error, (TimeoutError, socket.timeout)):
                status = '504 Gateway Timeout'
            elif isinstance(error, OSError) or diagnostic['phase'] in ('resolve', 'dial', 'upstream_handshake'):
                status = '502 Bad Gateway'
            else:
                status = '403 Forbidden'
            try:self.wfile.write(('HTTP/1.1 ' + status + '\r\nContent-Length: 0\r\nConnection: close\r\n\r\n').encode('ascii'))
            except OSError:pass
        finally:
            if remote:remote.close()
            diagnostic.update(duration_seconds=round(time.monotonic() - started, 6),
                              last_io_seconds=round(last_io - started, 6))
            try:
                print(json.dumps(diagnostic, separators=(',', ':')), file=sys.stderr, flush=True)
            except OSError:
                pass
    rbufsize=0

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address=True
    daemon_threads=True
if __name__=='__main__':
    Server(('0.0.0.0',3128),Handler).serve_forever()
