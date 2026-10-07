import socketserver


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        data = self.request.recv(1024)
        self.request.sendall(data)


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True


Server(('0.0.0.0', 8765), Echo).serve_forever()
