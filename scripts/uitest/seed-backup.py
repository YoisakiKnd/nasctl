"""Generate a new, marked local-backup test tree. Never reset existing data."""
import argparse
import json
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("root", type=Path)
parser.add_argument("--fixture-only", action="store_true", required=True)
args = parser.parse_args()
base = args.root.resolve()
if base.exists() and any(base.iterdir()):
    raise SystemExit("Refusing to modify a nonempty fixture directory")
for name in ["source/documents", "backup", "restored", "state", "cache"]:
    (base / name).mkdir(parents=True)
(base / "source" / ".nasctl-ui-fixture").write_text("nasctl-backup-fixture-v1\n")
(base / "source" / "documents" / "notes.txt").write_text("generated original backup bytes\n")
(base / "source" / "documents" / ".hidden").write_text("generated hidden bytes\n")
q = lambda value: json.dumps(str(value))
(base / "config.toml").write_text(f'''[server]
bind="127.0.0.1:18104"
[paths]
state_dir={q(base / "state")}
cache_dir={q(base / "cache")}
[hub.files]
copy_max_entries=100
copy_max_bytes=1048576
[[hub.storage]]
id="source"
label="Original files"
path={q(base / "source")}
[[hub.storage]]
id="backup"
label="Backup drive"
path={q(base / "backup")}
writable=true
[[hub.storage]]
id="restored"
label="Restored files"
path={q(base / "restored")}
writable=true
''')
print(base)
