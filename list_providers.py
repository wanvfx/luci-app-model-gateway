#!/usr/bin/env python3
"""List all providers from gateway config."""
import json, urllib.request, ssl

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE

req = urllib.request.Request("http://192.168.100.1:12211/api/config",
    headers={"Authorization": "Bearer sk-local-12da6905686cc1cacfde9cf7387b9bdf"})
with urllib.request.urlopen(req, timeout=15, context=ctx) as r:
    cfg = json.loads(r.read().decode())

ps = cfg.get("providers", [])
print(f"Total providers in config: {len(ps)}")
for p in ps:
    print(f"  id={p.get('id','?')} name={p.get('name','?')} base={p.get('base_url','?')} "
          f"auth_scheme={p.get('auth_scheme','?')} key={'***' if p.get('api_key') else '(empty)'} "
          f"models={len(p.get('models',[]))} format={p.get('format','?')}")
