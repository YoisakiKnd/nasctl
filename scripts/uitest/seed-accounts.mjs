// Create an entirely new fixture through the production initialization command.
import {execFileSync} from 'node:child_process';
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,realpathSync,chmodSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
const binary=process.argv[2],port=Number(process.argv[3]||18106);
if(!binary||!Number.isInteger(port)||port<1024||port>65535)throw Error('usage: seed-accounts.mjs BINARY [PORT]');
const base=realpathSync(mkdtempSync(join(tmpdir(),'nasctl-accounts-stage6-')));chmodSync(base,0o700);
mkdirSync(join(base,'data'),{mode:0o700});
for(const name of ['downloads','library'])mkdirSync(join(base,name),{mode:0o700});
writeFileSync(join(base,'data/generated.txt'),'original account fixture bytes\n');
writeFileSync(join(base,'data/.nasctl-ui-fixture'),'nasctl-accounts-fixture-v1\n');
const config=join(base,'config.toml');
execFileSync(binary,['--config',config,'init','--state-dir',join(base,'state'),'--bind',`127.0.0.1:${port}`,'--confirm'],{stdio:['ignore','pipe','pipe'],timeout:30000});
// Keep the first-setup path, with an idle native library to exercise viewer
// privacy in an actually configured module. No feed is fetched or downloaded.
const initial=readFileSync(config,'utf8'),start=initial.indexOf('[hub.anime]');
if(start<0)throw Error('initialized anime configuration section missing');
const following=initial.slice(start+1).search(/\n\s*\[/),end=following<0?initial.length:start+1+following;
let section=initial.slice(start,end);
for(const [name,value] of [['enabled','true'],['qbittorrent_url','"http://127.0.0.1:9999"'],['download_dir',JSON.stringify(join(base,'downloads'))],['library_dir',JSON.stringify(join(base,'library'))]]){
 const pattern=new RegExp('^(\\s*'+name+' = ).*$','m');if(!pattern.test(section))throw Error('missing initialized fixture field: '+name);section=section.replace(pattern,(_,prefix)=>prefix+value);
}
writeFileSync(config,initial.slice(0,start)+section+initial.slice(end),{mode:0o600});
writeFileSync(join(base,'fixture.json'),JSON.stringify({marker:'nasctl-accounts-fixture-v1',base:`http://127.0.0.1:${port}`,config,data:join(base,'data')}),{mode:0o600});
console.log(base);
