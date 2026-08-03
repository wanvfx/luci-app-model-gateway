# -*- coding: utf-8 -*-
"""ipk 结构校验：外层 gzip tar 三成员、Architecture、权限、htdocs/目录内容。"""
import sys, tarfile, io, json

path = sys.argv[1]
ok = True


def chk(cond, msg):
    global ok
    print(('  [PASS] ' if cond else '  [FAIL] ') + msg)
    if not cond:
        ok = False


with open(path, 'rb') as f:
    magic = f.read(2)
chk(magic == b'\x1f\x8b', 'outer is gzip (magic 1f 8b), got %r' % magic)

outer = tarfile.open(path, 'r:gz')
names = outer.getnames()
chk(len(names) == 3, 'outer has 3 members: %s' % names)
chk('./debian-binary' in names and './control.tar.gz' in names and './data.tar.gz' in names,
    'members have ./ prefix and correct names')

ctl = tarfile.open(fileobj=io.BytesIO(outer.extractfile('./control.tar.gz').read()), mode='r:gz')
control = ctl.extractfile('./control').read().decode('utf-8')
print('  --- control ---')
for line in control.strip().splitlines():
    print('     ' + line)
chk('Architecture: all' in control, 'Architecture: all')
chk('Version: 1.9.0-r20260802c' in control, 'Version matches r20260802c')
chk('Maintainer: Zoyaya' in control, 'Maintainer Zoyaya')

data = tarfile.open(fileobj=io.BytesIO(outer.extractfile('./data.tar.gz').read()), mode='r:gz')
members = data.getmembers()
print('  data members: %d' % len(members))
mp = {m.name.lstrip('./'): m for m in members}

for p in ['usr/bin/model-gatewayd', 'usr/bin/model-gatewayd-arm64',
          'etc/init.d/model-gateway', 'etc/init.d/model-gateway']:
    m = mp.get(p)
    if m:
        chk(m.mode == 0o755, '%s mode 755 (got %o)' % (p, m.mode))
    else:
        chk(False, '%s present' % p)

htdocs = [n for n in mp if 'htdocs/index.html' in n]
chk(len(htdocs) == 1, 'htdocs/index.html present: %s' % htdocs)
if htdocs:
    html = data.extractfile('./' + htdocs[0]).read().decode('utf-8')
    chk('keyfree_status' in html, 'htdocs contains keyfree_status (免Key实测状态徽章)')
    chk('KEYFREE_STATUS_META' in html, 'htdocs contains KEYFREE_STATUS_META')
    chk('可留空（免Key）' in html, 'htdocs keeps API-Key input for key-free providers')
    chk('1.9.0' in html, 'htdocs version 1.9.0')

cats = [n for n in mp if n.endswith('providers_catalog.json')]
chk(len(cats) == 1, 'providers_catalog.json present')
if cats:
    cat = json.loads(data.extractfile('./' + cats[0]).read().decode('utf-8'))
    ps = cat['providers']
    kf = [p for p in ps if p.get('auth') != 'apikey']
    annotated = [p for p in kf if p.get('keyfree_status')]
    chk(len(annotated) == len(kf), 'all %d key-free providers annotated (%d annotated)' % (len(kf), len(annotated)))
    unc = next((p for p in ps if p['id'] == 'uncloseai'), None)
    chk(unc and unc['models'][0]['id'] == 'solidrust/Hermes-3-Llama-3.1-8B-AWQ', 'uncloseai model id fixed')
    ovh = next((p for p in ps if p['id'] == 'ovhcloud'), None)
    chk(ovh and len(ovh['models']) == 12, 'ovhcloud has 12 models')
    hc = next((p for p in ps if p['id'] == 'hackclub'), None)
    chk(hc and hc['auth'] == 'apikey', 'hackclub relabeled to apikey')
    mm = next((p for p in ps if p['id'] == 'mimocode'), None)
    chk(mm and mm['base_url'] == 'https://api.xiaomimimo.com' and mm['auth'] == 'none' and mm.get('format') == 'mimocode',
        'mimocode base_url+auth+format fixed (免Key 适配器)')

chk(not any(n.endswith('.exe') for n in mp), 'no .exe in package')
chk(not any('Dockerfile' in n for n in mp), 'no Dockerfile in package')
chk(any('usr/lib/opkg/meta/model-gateway.json' in n for n in mp), 'iStore meta.json present')

print()
print('==== %s ====' % ('ALL CHECKS PASSED' if ok else 'SOME CHECKS FAILED'))
sys.exit(0 if ok else 1)
