"""Generate a new isolated settings fixture; never reset an existing directory."""
import argparse,json
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument('root',type=Path);p.add_argument('--fixture-only',action='store_true',required=True);args=p.parse_args();base=args.root.resolve()
if base.exists() and any(base.iterdir()):raise SystemExit('Refusing nonempty fixture')
for name in ['data','state','cache']:(base/name).mkdir(parents=True,mode=0o700)
(base/'data/.nasctl-ui-fixture').write_text('nasctl-settings-fixture-v1\n')
(base/'data/generated.txt').write_text('original settings fixture bytes\n')
q=lambda value:json.dumps(str(value))
(base/'config.toml').write_text(f'''[server]
bind="127.0.0.1:18105"
[paths]
state_dir={q(base/'state')}
cache_dir={q(base/'cache')}
[hub.anime]
password="fixture-initial-private-password"
tmdb_token="fixture-initial-private-tmdb"
[[hub.storage]]
id="data"
label="Generated files"
path={q(base/'data')}
''')
(base/'config.toml').chmod(0o600)
print(base)
