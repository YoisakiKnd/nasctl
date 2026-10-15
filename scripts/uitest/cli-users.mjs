// Independent production CLI processes, restricted to newly generated private
// fixture paths. Passwords go to stdin and are never printed or passed as argv.
import {execFileSync} from 'node:child_process';
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,existsSync,realpathSync,chmodSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
const binary=process.argv[2];if(!binary)throw Error('usage: cli-users.mjs BINARY');
const base=realpathSync(mkdtempSync(join(tmpdir(),'nasctl-cli-users-')));chmodSync(base,0o700);
const source=join(base,'config.toml'),state=join(base,'state');
const password='generated independent CLI password';
const check=(ok,label)=>{if(!ok)throw Error(label);console.log('ok:',label)};
function run(args,{ok=true,input=''}={}){try{const text=execFileSync(binary,['--config',source,'--json',...args],{input,encoding:'utf8',timeout:60000,stdio:['pipe','pipe','pipe']});check(!text.includes(password),'CLI omits password');if(!ok)throw Error('unexpected CLI success');return text.trim().startsWith('{')?JSON.parse(text):text;}catch(e){if(ok||!e.status)throw e;check(!String(e.stderr).includes(password),'failed CLI omits password');return e;}}
run(['init','--state-dir',state],{ok:false});check(!existsSync(source),'unconfirmed initialization does not write');
run(['init','--state-dir',state,'--confirm']);const baseConfig=readFileSync(source);run(['init','--state-dir',state,'--confirm'],{ok:false});check(readFileSync(source).equals(baseConfig),'initialization never overwrites config');
run(['users','create','--username','owner','--role','admin','--password-stdin'],{ok:false,input:password+'\n'});check(!existsSync(join(state,'nasctl-users.json')),'unconfirmed account creation does not write');
run(['users','create','--username','owner','--role','admin','--password-stdin','--confirm'],{input:password+'\n'});
run(['users','setup-code'],{ok:false});
let listed=run(['users','create','--username','reader','--password-stdin','--confirm'],{input:password+'\n'});check(listed.users.find(u=>u.username==='reader')?.role==='viewer','new member defaults to viewer');
const oldOwner=listed.users.find(u=>u.username==='owner').id;
run(['mode','pause']);run(['users','password','--username','owner','--password-stdin','--confirm'],{input:password+' changed\n'});check(run(['mode','show']).effective.mode==='manual_pause','password maintenance preserves paused mode');
run(['users','remove','--username','owner','--confirm-username','owner','--confirm'],{ok:false});check(run(['users','list']).users.length===2,'last enabled administrator preserved');
run(['--agent','fixture-agent','users','recover','--username','owner','--password-stdin','--confirm'],{ok:false,input:password+'\n'});
writeFileSync(join(state,'nasctl-settings.json'),'damaged saved settings fixture',{mode:0o600});writeFileSync(join(state,'hub.db'),'damaged job database fixture',{mode:0o600});
run(['settings','reset'],{ok:false});const reset=run(['settings','reset','--confirm']);check(readFileSync(join(reset.quarantine,'original.raw'),'utf8')==='damaged saved settings fixture','settings reset preserves corrupt original despite damaged task database');
writeFileSync(join(state,'nasctl-users.json'),'damaged account fixture',{mode:0o600});run(['users','list'],{ok:false});
const recovery=run(['users','recover','--username','recovered','--password-stdin','--confirm'],{input:password+'\n'});check(recovery.completed&&readFileSync(join(recovery.quarantine,'original.raw'),'utf8')==='damaged account fixture','offline account repair preserves corruption');
listed=run(['users','list']);check(listed.users.length===1&&listed.users[0].username==='recovered'&&listed.users[0].role==='admin'&&listed.users[0].id!==oldOwner,'offline recovery replaces identities explicitly');
check(readFileSync(source).equals(baseConfig)&&readFileSync(join(state,'hub.db'),'utf8')==='damaged job database fixture','repairs preserve base TOML and unrelated database');
console.log('Independent CLI initialization, accounts, paused maintenance and corrupt-state repairs passed. Fixture: '+base);
