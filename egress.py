"""Public-only HTTP/CONNECT proxy. Resolve once, validate all, dial pinned IP."""
import ipaddress, socket, socketserver, select, urllib.parse, os

def upstream_tunnel(address, value):
    u = urllib.parse.urlsplit(value)
    if u.scheme != 'http' or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('', '/'):
        raise ValueError('invalid trusted upstream')
    s = socket.create_connection((u.hostname, u.port or 80), timeout=15)
    try:
        host = '[' + address[0] + ']' if ':' in address[0] else address[0]
        authority = host + ':' + str(address[1])
        s.sendall(('CONNECT ' + authority + ' HTTP/1.1\r\nHost: ' + authority + '\r\n\r\n').encode('ascii'))
        header = b''
        while not header.endswith(b'\r\n\r\n'):
            b = s.recv(1)
            if not b or len(header) >= 32768:
                raise OSError('invalid upstream response')
            header += b
        status = header.split(b'\r\n', 1)[0].split()
        if len(status) < 2 or status[1] != b'200':
            raise OSError('upstream refused connection')
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

def connect(host, port):
    if port not in (80, 443):
        raise ValueError('port denied')
    answers = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    if not answers or any(not public(a[4][0]) for a in answers):
        raise ValueError('destination denied')
    upstream = os.environ.get('PUBLIC_EGRESS_UPSTREAM_PROXY', '')
    for family, kind, proto, _, address in answers:
        if upstream:
            try:
                return upstream_tunnel(address, upstream)
            except OSError:
                continue
        s = socket.socket(family, kind, proto); s.settimeout(15)
        try:
            s.connect(address); return s
        except OSError:
            s.close()
    raise OSError('connection failed')

class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        remote = None
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
                remote=connect(u.hostname, u.port or 443)
                self.wfile.write(b'HTTP/1.1 200 Connection Established\r\n\r\n');self.wfile.flush()
            else:
                u=urllib.parse.urlsplit(target)
                if u.scheme!='http' or u.username or u.password: raise ValueError('scheme')
                remote=connect(u.hostname,u.port or 80)
                path=u.path or '/'
                if u.query: path+='?'+u.query
                remote.sendall((method+' '+path+' '+version+'\r\n').encode('ascii'))
                remote.sendall(b''.join(h for h in headers if not h.lower().startswith((b'proxy-',b'connection:')))+b'Connection: close\r\n\r\n')
            # HTTP downloads and CONNECT only; POST bodies are relayed from the
            # unbuffered input stream so no buffered bytes get lost.
            while True:
                ready,_,_=select.select([self.connection,remote],[],[],90)
                if not ready: break
                for src in ready:
                    data=src.recv(65536)
                    if not data:return
                    (remote if src is self.connection else self.connection).sendall(data)
        except (OSError,ValueError,UnicodeError):
            try:self.wfile.write(b'HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n')
            except OSError:pass
        finally:
            if remote:remote.close()
    rbufsize=0

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address=True
    daemon_threads=True
if __name__=='__main__':
    Server(('0.0.0.0',3128),Handler).serve_forever()
