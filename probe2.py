import json, ssl, urllib.request, urllib.error

ctx = ssl._create_unverified_context()


def get(url, timeout=20):
    try:
        r = urllib.request.Request(url, headers={'User-Agent': 'Mozilla/5.0', 'Accept': 'application/json'})
        with urllib.request.urlopen(r, timeout=timeout, context=ctx) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()[:300]
    except Exception as e:
        return None, str(e).encode()[:200]


targets = {
    'mimocode-root': 'https://api.xiaomimimo.com/models',
    'mimocode-v1': 'https://api.xiaomimimo.com/v1/models',
    'ovhcloud': 'https://oai.endpoints.kepler.ai.cloud.ovh.net/v1/models',
    'uncloseai': 'https://hermes.ai.unturf.com/v1/models',
    'hackclub': 'https://ai.hackclub.com/proxy/v1/models',
}
out = {}
for k, u in targets.items():
    st, body = get(u)
    print('==', k, u, 'st=', st)
    ids = []
    try:
        j = json.loads(body)
        data = j.get('data') if isinstance(j, dict) else j
        if isinstance(data, list):
            ids = [d.get('id') for d in data if isinstance(d, dict) and d.get('id')]
    except Exception:
        print('   parse-fail:', body[:160])
    print('   models:', len(ids), ids[:12])
    out[k] = {'status': st, 'ids': ids}

json.dump(out, open('probe2.json', 'w', encoding='utf-8'), ensure_ascii=False, indent=1)
print('saved probe2.json')
