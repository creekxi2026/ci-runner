#!/usr/bin/env python3
"""Bounded, read-only probes for the exact public inputs in Dockerfile.

This is early failure detection, not signature verification or proof that a
later download/push will succeed. Docker/apt still verify and fetch real inputs.
"""
import concurrent.futures
import json
import hashlib
from pathlib import Path
import re
import subprocess
import tempfile


def inputs(dockerfile):
    images = re.findall(r'^FROM (\S+)', dockerfile, re.M)
    if not images or any(not re.fullmatch(r'[a-z0-9./:_-]+@sha256:[a-f0-9]{64}', i)
                         for i in images):
        raise ValueError('every external FROM must be digest pinned')
    sources = re.findall(r"'deb \[check-valid-until=no\] (https://[^ ]+) ([a-z-]+) [^']+'", dockerfile)
    # Fail closed if Dockerfile source syntax changes: do not silently omit it.
    if not sources or len(sources) != dockerfile.count("'deb "):
        raise ValueError('unsupported or missing snapshot source declaration')
    urls = []
    for base, suite in sources:
        if not re.fullmatch(r'https://snapshot\.(?:ubuntu\.com/ubuntu|debian\.org/archive/(?:debian|debian-security))/[0-9]{8}T[0-9]{6}Z/', base):
            raise ValueError('snapshot must be an explicit fixed HTTPS input')
        urls.append(base + 'dists/' + suite + '/InRelease')
    return [('manifest', i) for i in dict.fromkeys(images)] + [('snapshot', u) for u in dict.fromkeys(urls)]


def probe_snapshot(url):
    with tempfile.TemporaryDirectory(prefix='image-preflight-') as directory:
        body = Path(directory) / 'InRelease'
        result = subprocess.run([
            'curl', '--silent', '--show-error', '--fail', '--location',
            '--proto', '=https', '--proto-redir', '=https', '--max-redirs', '3',
            '--connect-timeout', '5', '--max-time', '15', '--retry', '0',
            '--max-filesize', '2097152', '--output', str(body), url,
        ], capture_output=True, timeout=20)
        if result.returncode:
            raise ValueError('GET failed: ' + result.stderr.decode(errors='replace')[:300].strip())
        content = body.read_bytes()
        if not (content.startswith(b'-----BEGIN PGP SIGNED MESSAGE-----')
                and b'\nSuite: ' in content and b'\nSHA256:\n' in content
                and b'-----BEGIN PGP SIGNATURE-----' in content):
            raise ValueError('GET did not return signed release metadata (possibly an HTML error page)')


def probe_manifest(reference):
    result = subprocess.run(['docker', 'buildx', 'imagetools', 'inspect', '--raw', reference],
                            capture_output=True, timeout=20)
    if result.returncode:
        # Public references only; never print Docker credential/helper output.
        raise ValueError('registry manifest read failed (exit %s)' % result.returncode)
    manifest = json.loads(result.stdout)
    if (not isinstance(manifest, dict) or manifest.get('schemaVersion') != 2
            or not (manifest.get('manifests') or manifest.get('layers'))):
        raise ValueError('registry returned an invalid image manifest')


def check(item):
    kind, target = item
    try:
        (probe_snapshot if kind == 'snapshot' else probe_manifest)(target)
        return {'kind': kind, 'target': target, 'ok': True}
    except (ValueError, OSError, subprocess.TimeoutExpired) as error:
        return {'kind': kind, 'target': target, 'ok': False,
                'error': 'probe timeout' if isinstance(error, subprocess.TimeoutExpired) else str(error)}


def verify_local_module(root=Path('.')):
    """Authenticate the explicit local source closure before any image build."""
    module=root/'third_party/scaleset'
    source=json.loads((module/'UPSTREAM.json').read_bytes())
    if source['module']!='github.com/actions/scaleset' or source['version']!='v0.4.1-0.20260916214619-e6daac702355' or source['upstream_h1']!='h1:+DtFsbhUIxDt1vJL+xMPCPTZzE7nw+K4KTqqwUOFqIc=':
        raise ValueError('local module upstream identity differs')
    expected=dict(source['files'])
    for name,delta in source['patch'].items():
        if delta['upstream_sha256']!=expected.get(name):raise ValueError('local module delta origin differs')
        expected[name]=delta['local_sha256']
    actual={str(p.relative_to(module)):hashlib.sha256(p.read_bytes()).hexdigest() for p in module.rglob('*') if p.is_file() and p.name!='UPSTREAM.json'}
    if actual!=expected:raise ValueError('local module source closure differs')
    if actual['LICENSE']!=source['license_sha256']:raise ValueError('upstream LICENSE differs')
    dockerfile=(root/'Dockerfile').read_text()
    copy='COPY third_party/scaleset ./third_party/scaleset'
    if copy not in dockerfile or dockerfile.index(copy)>dockerfile.index('RUN go mod download'):raise ValueError('Docker local replacement closure missing')
    if 'replace github.com/actions/scaleset => ./third_party/scaleset' not in (root/'go.mod').read_text():raise ValueError('explicit local replace missing')
    return len(actual)


def main():
    closure=verify_local_module()
    targets = inputs(Path('Dockerfile').read_text())
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        results = list(pool.map(check, targets))
    print(json.dumps({'scope': 'public input availability and local source closure', 'local_module_files': closure, 'checks': results}, indent=2))
    return 0 if all(item['ok'] for item in results) else 1


if __name__ == '__main__':
    raise SystemExit(main())
