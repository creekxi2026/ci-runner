#!/usr/bin/python3
import hashlib
import json
import os
from pathlib import Path
import socket
import sys
import time

request = json.loads(Path('/run/ci-adapter/request.json').read_text())
output = Path('/outputs/consumer.json')
mode = request['config'].get('mode', 'ok')
if sys.argv[1] == 'init':
    bootstrap = Path(request['secret_files']['bootstrap']).read_bytes()
    # Deliberate canary: controller/worker must never relay adapter output.
    print(bootstrap.decode(), flush=True)
    if mode == 'fail':
        sys.exit(17)
    if mode == 'hang' or (mode == 'resume' and not output.exists()):
        output.write_text('{"checkpoint":true}')
        time.sleep(120)
    if mode == 'symlink':
        output.symlink_to('/etc/passwd')
        sys.exit(0)
    output.write_text(json.dumps({'lease_id': request['lease_id'],
                                 'endpoint': request['endpoint'],
                                 'token': hashlib.sha256(bootstrap + b'consumer').hexdigest()}))
    output.chmod(0o400)
else:
    assert request['secret_files'] == {}
    assert not Path('/run/ci-service-secrets').exists()
    assert not os.access('/outputs', os.W_OK)
    if mode == 'symlink':
        # A malicious/broken check reporting success must not bypass the
        # controller's independent regular-file/publication validation.
        sys.exit(0)
    payload = json.loads(output.read_text())
    assert payload['lease_id'] == request['lease_id']
for attempt in range(100):
    try:
        with socket.create_connection((request['endpoint']['host'], 8765), timeout=0.2) as conn:
            conn.sendall(b'lease-check')
            assert conn.recv(1024) == b'lease-check'
        break
    except OSError:
        time.sleep(0.1)
else:
    sys.exit(18)
