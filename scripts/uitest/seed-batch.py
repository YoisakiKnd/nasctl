"""Create a new, explicitly marked synthetic file-batch fixture; never reset data."""
import argparse
import json
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("root", type=Path)
parser.add_argument("--fixture-only", action="store_true", required=True)
args = parser.parse_args()
root = args.root.resolve()
if root.exists() and any(root.iterdir()):
    raise SystemExit("Refusing to modify an existing nonempty fixture directory")
root.mkdir(parents=True, exist_ok=True)
for part in ["files", "readonly", "state", "cache"]:
    (root / part).mkdir()
(root / "files" / "target").mkdir()
for name in ["Alpha.txt", "Beta.txt", "Gamma.txt"]:
    (root / "files" / name).write_text("nasctl-generated-" + name + "\n")
(root / "files" / "target" / "Alpha.txt").write_text("existing-generated-destination\n")
(root / "files" / ".nasctl-ui-fixture").write_text("nasctl-batch-fixture-v1\n")
(root / "readonly" / "Read.txt").write_text("generated-readonly\n")
q = lambda value: json.dumps(str(value))
(root / "config.toml").write_text(f'''[server]
bind="127.0.0.1:18103"
[paths]
state_dir={q(root / "state")}
cache_dir={q(root / "cache")}
[hub.files]
batch_max_items=4
copy_max_entries=50
copy_max_bytes=4194304
upload_max_bytes=1048576
[[hub.storage]]
id="data"
label="Batch Fixture"
path={q(root / "files")}
writable=true
[[hub.storage]]
id="readonly"
label="Readonly Fixture"
path={q(root / "readonly")}
''')
print(root)
