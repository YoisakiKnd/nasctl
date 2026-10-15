"""Run inside an owned Linux systemd fixture, never against a deployed NAS."""
import argparse,hashlib,json,sqlite3,time,urllib.request
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument('root',type=Path);p.add_argument('--fixture-only',action='store_true',required=True);args=p.parse_args();root=args.root.resolve()
if root.parent!=Path('/run') or not root.name.startswith('nasctl-kernel-album-') or (root/'.nasctl-fixture').read_text()!='nasctl-kernel-album-v1\n':raise SystemExit('owned /run fixture required')
base='http://127.0.0.1:18120'
def request(path,body=None):
    headers={'Authorization':'Bearer nasctl-kernel-album-fixture-token'}
    if body is not None:headers.update({'Content-Type':'application/json','X-Nasctl-Confirm':'1'})
    req=urllib.request.Request(base+path,data=json.dumps(body).encode() if body is not None else None,headers=headers)
    with urllib.request.urlopen(req,timeout=10) as response:return json.load(response)
def run(operation):
    started=request('/api/v1/album/tasks/'+operation,{'collection':'photos','limit':3,'max_runtime':'2m'})
    for _ in range(600):
        data=request('/api/v1/jobs/'+started['job_id']);job=data['job']
        if job['state'] not in ['queued','running','waiting','paused']:
            if job['state']!='completed':raise AssertionError(json.dumps(data,ensure_ascii=False))
            print('ok:',operation,job['state'],flush=True);return
        time.sleep(.2)
    raise AssertionError('bounded fixture task timed out')
before={f.name:hashlib.sha256(f.read_bytes()).hexdigest() for f in (root/'photos').glob('generated-*.jpg')}
assert len(before)==3
run('scan');run('thumbs');run('thumbs') # Second task proves empty subtree reuse.
db=sqlite3.connect('file:'+str(root/'state/index.sqlite3')+'?mode=ro',uri=True)
assert db.execute('pragma quick_check').fetchone()[0]=='ok'
assert db.execute("select count(*) from media where thumb_state='ok'").fetchone()[0]==3
tasks=db.execute("select cgroup_isolated,report,processed,bytes_written from tasks where kind='thumbs' order by id desc limit 2").fetchall();history=db.execute("select id,cgroup_isolated,processed,bytes_written from tasks where kind='thumbs' order by id").fetchall();db.close()
assert len(tasks)==2 and all(row[0]==1 for row in tasks),tasks
reports=[row[1] for row in tasks]
assert all('worker/worker' in str(row) for row in reports),reports
after={f.name:hashlib.sha256(f.read_bytes()).hexdigest() for f in (root/'photos').glob('generated-*.jpg')};assert before==after
(root/'kernel-proof.json').write_text(json.dumps({'thumbnails':3,'isolated_tasks':2,'originals_unchanged':True,'reports':reports,'task_history':history},ensure_ascii=False))
print('ok: three actual thumbnails, two isolated NAS-launched tasks, original bytes unchanged',flush=True)
