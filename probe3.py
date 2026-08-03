import json, ssl, urllib.request, urllib.error

ctx = ssl._create_unverified_context()


def post(url, payload, timeout=40, headers=None):
    h = {'Content-Type': 'application/json', 'User-Agent': 'Mozilla/5.0', 'Accept': 'application/json'}
    if headers:
        h.update(headers)
    try:
        r = urllib.request.Request(url, data=json.dumps(payload).encode(), headers=h, method='POST')
        with urllib.request.urlopen(r, timeout=timeout, context=ctx) as resp:
            return resp.status, resp.read()[:400]
    except urllib.error.HTTPError as e:
        return e.code, e.read()[:400]
    except Exception as e:
        return None, str(e).encode()[:200]


cases = [
    ('ovhcloud/Qwen3-32B', 'https://oai.endpoints.kepler.ai.cloud.ovh.net/v1/chat/completions', 'Qwen3-32B'),
    ('ovhcloud/gpt-oss-120b', 'https://oai.endpoints.kepler.ai.cloud.ovh.net/v1/chat/completions', 'gpt-oss-120b'),
    ('uncloseai/AWQ', 'https://hermes.ai.unturf.com/v1/chat/completions', 'solidrust/Hermes-3-Llama-3.1-8B-AWQ'),
]
res = {}
for name, url, model in cases:
    st, body = post(url, {'model': model, 'messages': [{'role': 'user', 'content': 'hi'}], 'max_tokens': 8})
    print('==', name, 'st=', st)
    print('   ', body[:220])
    res[name] = st
print()
print('汇总:', res)
