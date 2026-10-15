// Compatible-version rollback on generated data only. The candidate must
// understand the checkpoint: this is not an arbitrary schema downgrade test.
import {execFileSync,spawn} from 'node:child_process';
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,realpathSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import net from 'node:net';
const [current,candidate]=process.argv.slice(2);if(!current||!candidate)throw Error('Pass current and compatible candidate binaries');
const base=realpathSync(mkdtempSync(join(tmpdir(),'nasctl-upgrade-rollback-')));
for(const dir of ['state','cache','files'])mkdirSync(join(base,dir),{mode:0o700});
const server=net.createServer();await new Promise(r=>server.listen(0,'127.0.0.1',r));const port=server.address().port;await new Promise(r=>server.close(r));
const config=join(base,'config.toml'),pack=join(base,'before-upgrade'),token='generated-rollback-fixture-token';
writeFileSync(join(base,'files/original.txt'),'generated rollback original bytes\n',{mode:0o600});
writeFileSync(config,`[server]\nbind="127.0.0.1:${port}"\ntoken="${token}"\n[paths]\nstate_dir=${JSON.stringify(join(base,'state'))}\ncache_dir=${JSON.stringify(join(base,'cache'))}\n[[hub.storage]]\nid="data"\npath=${JSON.stringify(join(base,'files'))}\n`,{mode:0o600});
const original=readFileSync(config),check=(ok,label)=>{if(!ok)throw Error(label);console.log('ok:',label)};
function run(bin,args){const out=execFileSync(bin,['--config',config,'--json',...args],{encoding:'utf8',timeout:60000,stdio:['ignore','pipe','pipe']});return out.trim().startsWith('{')?JSON.parse(out):out;}
const pause=ms=>new Promise(r=>setTimeout(r,ms));
async function boot(bin,expected){
 const child=spawn(bin,['serve','--config',config],{stdio:['ignore','pipe','pipe']});let output='';for(const stream of [child.stdout,child.stderr])stream.on('data',b=>{output=(output+b).slice(-8192)});
 const exited=new Promise(resolve=>child.once('exit',resolve));
 try{
  let ready=false;for(let i=0;i<100;i++){if(child.exitCode!==null)throw Error(output);try{const r=await fetch(`http://127.0.0.1:${port}/api/v1/overview`,{headers:{Authorization:'Bearer '+token}});if(r.ok){ready=true;break}}catch{}await pause(50)}
  check(ready,'real service starts after '+expected);
  const r=await fetch(`http://127.0.0.1:${port}/api/v1/files/download?root=data&path=original.txt`,{headers:{Authorization:'Bearer '+token}});check(r.ok&&await r.text()==='generated rollback original bytes\n','original bytes readable after '+expected);
 }finally{if(child.exitCode===null)child.kill('SIGTERM');await Promise.race([exited,pause(5000)]);if(child.exitCode===null){child.kill('SIGKILL');throw Error('service failed graceful shutdown')}}
}
run(candidate,['mode','show']);await boot(candidate,'candidate initialization');
const checkpoint=run(current,['state','backup','--package',pack,'--confirm']);
check(run(candidate,['state','verify','--package',pack]).manifest_sha256===checkpoint.manifest_sha256,'candidate independently verifies upgrade checkpoint');
run(current,['mode','pause']);check(run(current,['mode','show']).effective.mode==='manual_pause','current binary changes state');await boot(current,'upgrade');
const beforeRollback=run(current,['state','backup','--package',join(base,'before-rollback'),'--confirm']);check(beforeRollback.files>0,'newer state checkpoint preserved');
const restored=run(current,['state','restore','--package',pack,'--manifest-sha256',checkpoint.manifest_sha256,'--confirm']);
check(restored.checkpoint&&run(current,['mode','show']).effective.mode==='normal','restore creates checkpoint and returns saved state');
check(run(candidate,['mode','show']).effective.mode==='normal','candidate understands restored state');await boot(candidate,'compatible rollback');
check(readFileSync(config).equals(original),'active base configuration remains byte-identical');
console.log('Compatible upgrade/rollback verified; generated private fixture retained at '+base);
