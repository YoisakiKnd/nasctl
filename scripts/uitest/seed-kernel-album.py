"""Seed a new owned /run fixture for delegated native-album verification."""
import argparse,json,os,re,shutil,stat
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument('root',type=Path);p.add_argument('--binary-dir',type=Path,required=True);p.add_argument('--source',type=Path,required=True);p.add_argument('--fixture-only',action='store_true',required=True);args=p.parse_args()
root=args.root.resolve();source=args.source.resolve()
if root.parent!=Path('/run') or not re.fullmatch(r'nasctl-kernel-album-[A-Za-z0-9_-]+',root.name) or not root.is_dir() or list(root.iterdir()) or root.stat().st_uid!=os.getuid() or stat.S_IMODE(root.stat().st_mode)!=0o700:raise SystemExit('new private owned /run fixture required')
if (source/'.nasctl-source-fixture').read_text()!='nasctl-kernel-source-v1\n':raise SystemExit('generated source fixture required')
os.umask(0o077)
for name in ['bin','photos','state','cache']:(root/name).mkdir(mode=0o750)
for name in ['nasctl','albumctl']:
    binary=(args.binary_dir/name).resolve()
    if not binary.is_file() or binary.stat().st_size>128<<20 or not binary.stat().st_mode&0o111:raise SystemExit('built executable fixture binaries required')
    with binary.open('rb') as src,(root/'bin'/name).open('xb') as dst:shutil.copyfileobj(src,dst,65536)
    (root/'bin'/name).chmod(0o755)
for index,name in enumerate(['photos/2018/IMG_0001.jpg','photos/2019/paris/IMG_0007.jpg','photos/2021/trip/IMG_0100.jpg']):
    with (source/'synth'/name).open('rb') as src,(root/'photos'/('generated-'+str(index)+'.jpg')).open('xb') as dst:shutil.copyfileobj(src,dst,65536)
(root/'.nasctl-fixture').write_text('nasctl-kernel-album-v1\n')
q=json.dumps
(root/'config.toml').write_text('[server]\nbind="127.0.0.1:18120"\ntoken="nasctl-kernel-album-fixture-token"\n[paths]\nstate_dir='+q(str(root/'state'))+'\ncache_dir='+q(str(root/'cache'))+'\n[[hub.storage]]\nid="photos"\npath='+q(str(root/'photos'))+'\n[[collections]]\ncode="photos"\nname="Kernel Fixture"\nroot='+q(str(root/'photos'))+'\nenabled=true\nallow_shared_volume=true\n')
template=(Path(__file__).resolve().parents[2]/'systemd/nasctl-web.service').read_text()
override='\n[Service]\nUser='+str(os.getuid())+'\nGroup='+str(os.getgid())+'\nExecStart=\nExecStart='+str(root/'bin/nasctl')+' serve --config '+str(root/'config.toml')+'\nStateDirectory=\nCacheDirectory=\nReadWritePaths=\nReadWritePaths='+str(root/'state')+' '+str(root/'cache')+'\n'
(root/'unit.service').write_text(template+override)
print(root)
