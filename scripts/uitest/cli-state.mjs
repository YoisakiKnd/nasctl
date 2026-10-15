// Independent CLI processes; only freshly generated private fixture data.
import {execFileSync} from 'node:child_process';
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,existsSync,realpathSync,chmodSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
const binary=process.argv[2];if(!binary)throw Error('Pass a built nasctl binary.');
const base=realpathSync(mkdtempSync(join(tmpdir(),'nasctl-cli-state-')));chmodSync(base,0o700);
for(const dir of ['data','state','cache'])mkdirSync(join(base,dir),{mode:0o700});
const config=join(base,'config.toml'),pack=join(base,'package');
writeFileSync(config,`[paths]\nstate_dir=${JSON.stringify(join(base,'state'))}\ncache_dir=${JSON.stringify(join(base,'cache'))}\n[[hub.storage]]\nid="data"\npath=${JSON.stringify(join(base,'data'))}\n`,{mode:0o600});
const initialConfig=readFileSync(config);
const check=(ok,label)=>{if(!ok)throw Error(label);console.log('ok:',label);};
function run(args,success=true){try{const out=execFileSync(binary,['--config',config,'--json',...args],{encoding:'utf8',timeout:60000,stdio:['ignore','pipe','pipe']});if(!success)throw Error('unexpected CLI success');return out.trim().startsWith('{')?JSON.parse(out):out;}catch(e){if(success||!e.status)throw e;return e;}}
run(['mode','show']);
run(['state','backup','--package',pack],false);check(!existsSync(pack),'unconfirmed state backup creates no package');
run(['--agent','fixture-agent','state','backup','--package',pack,'--confirm'],false);check(!existsSync(pack),'agent cannot export credential-bearing state');
const made=run(['state','backup','--package',pack,'--confirm']);check(made.files===2&&made.manifest_sha256?.length===64,'offline CLI creates verified hub/config package');
const checked=run(['state','verify','--package',pack]);check(checked.manifest_sha256===made.manifest_sha256,'independent CLI verifies same manifest');
run(['mode','pause']);check(run(['mode','show']).effective.mode==='manual_pause','newer state exists before restore');
run(['state','restore','--package',pack,'--manifest-sha256','0'.repeat(64),'--confirm'],false);check(run(['mode','show']).effective.mode==='manual_pause','wrong digest preserves newer state');
const restored=run(['state','restore','--package',pack,'--manifest-sha256',checked.manifest_sha256,'--confirm']);
check(run(['mode','show']).effective.mode==='normal','restored state returns to backed-up mode');
check(restored.checkpoint&&run(['state','verify','--package',restored.checkpoint]).files===2,'pre-restore checkpoint independently verifies');
check(readFileSync(config).equals(initialConfig)&&restored.config_restored===false,'active configuration bytes preserved');
const audit=JSON.parse(readFileSync(join(base,'state/offline-state-audit.json'),'utf8'));
check(audit.some(r=>r.action==='backup'&&r.outcome==='ok')&&audit.some(r=>r.action==='restore'&&r.outcome==='ok'),'offline operations have separate durable bounded audit');
console.log('CLI offline state workflow passed; private generated fixture retained at '+base);
