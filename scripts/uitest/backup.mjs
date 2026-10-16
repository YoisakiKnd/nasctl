// Compiled Svelte + actual nasctl. All mutations require a marked synthetic root.
import {JSDOM,VirtualConsole} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18104';
const requests=[];
const request=(path,options={})=>{requests.push({path:String(path),method:options.method||'GET'});return fetch(new URL(path,base),options);};
const marker=await request('/api/v1/files/download?root=source&path=.nasctl-ui-fixture');if(!marker.ok||await marker.text()!=='nasctl-backup-fixture-v1\n')throw Error('Refusing an unmarked fixture');
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',(e)=>errors.push(e.message));
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#backup',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise(r=>setTimeout(r,ms));
const check=(ok,label)=>{if(!ok)throw Error(label);console.log('ok:',label);};
async function wait(test,label){const end=Date.now()+10000;while(Date.now()<end){if(test())return;await delay(25);}throw Error('timed out: '+label+' '+d.querySelector('[role="alert"]')?.textContent);}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find(b=>b.textContent.trim()===label);
const click=(label,scope=d)=>{const b=button(label,scope);if(!b||b.disabled)throw Error('button unavailable: '+label);b.click();};
const field=(label,value)=>{const f=d.querySelector('[aria-label="'+label+'"]');if(!f)throw Error('missing '+label);f.value=value;f.dispatchEvent(new w.Event('input',{bubbles:true}));f.dispatchEvent(new w.Event('change',{bubbles:true}));};
const submit=()=>d.querySelector('dialog .backup-form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
const calls=(suffix)=>requests.filter(r=>r.method==='POST'&&r.path==='/api/v1/backup/'+suffix).length;
async function taskFinished(op){await wait(()=>d.querySelector('.backup-result')&&!d.querySelector('dialog')&&d.querySelector('.task-monitor strong')?.textContent==='backup.'+op,'task '+op);await wait(()=>button('执行备份')&&!button('执行备份').disabled,'task settled '+op);}
const content=async(root,path)=>{const r=await request('/api/v1/files/download?'+new URLSearchParams({root,path}));if(!r.ok)throw Error(await r.text());return r.text();};
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
try{
 w.eval(await(await request('/app.js')).text());await wait(()=>button('本地副本'),'backup tabs');click('本地副本');await wait(()=>button('新建计划')&&!button('新建计划').disabled,'backup view');
 check(!d.body.textContent.includes('备份服务尚未接入'),'native backup replaces placeholder');
 click('新建计划');await wait(()=>d.querySelector('dialog[open]'),'plan form');
 field('备份计划编号','generated');field('备份计划名称','Generated backup');field('备份源存储目录','source');field('备份源相对位置','documents');field('备份目标存储目录','backup');field('备份保留份数','3');field('备份条目上限','50');field('备份字节上限','1048576');field('备份最长运行时间','1m');
 check(calls('plans')===0,'plan not saved before confirmation');await delay(0);check(d.querySelector('dialog .backup-form').checkValidity(),'backup plan native form validity');submit();await wait(()=>d.querySelector('.backup-plan')?.textContent.includes('Generated backup')&&!d.querySelector('dialog'),'saved plan');
 click('执行备份');await wait(()=>d.querySelector('dialog[open]'),'run confirmation');check(calls('run')===0,'no backup copy before confirmation');submit();
 await taskFinished('run');await wait(()=>d.querySelectorAll('.backup-version').length===1,'published version');
 check(d.querySelector('.backup-result').textContent.includes('备份版本已保存'),'actual published result');
 const history=await(await request('/api/v1/backup/history?root=backup')).json();const version=history.snapshots[0];check(version?.bytes>0&&version.manifest_sha256.length===64,'real bounded snapshot metadata');
 click('校验',d.querySelector('.backup-version'));await wait(()=>d.querySelector('dialog[open]'),'verify confirmation');submit();await taskFinished('verify');check(d.querySelector('.backup-result').textContent.includes('校验通过'),'manifest and file verification');
 click('恢复',d.querySelector('.backup-version'));await wait(()=>d.querySelector('dialog[open]'),'restore form');field('备份恢复目标存储目录','restored');field('备份恢复目标位置','restored-copy');submit();await taskFinished('restore');
 check(d.querySelector('.backup-result').textContent.includes('恢复目标已保存'),'restore published after staged verification');
 const original=await content('source','documents/notes.txt');check(await content('restored','restored-copy/notes.txt')===original,'restored original bytes');check(await content('restored','restored-copy/.hidden')===await content('source','documents/.hidden'),'hidden bytes included');
 click('恢复',d.querySelector('.backup-version'));await wait(()=>d.querySelector('dialog[open]'),'conflict restore form');field('备份恢复目标存储目录','restored');field('备份恢复目标位置','restored-copy');submit();await taskFinished('restore');
 check(d.querySelector('.backup-result').textContent.includes('任务未全部完成'),'existing target conflict shown');check(await content('restored','restored-copy/notes.txt')===original,'conflict does not overwrite destination');
 click('删除副本',d.querySelector('.backup-version'));await wait(()=>d.querySelector('dialog[open]'),'delete confirmation');field('备份清理确认编号','wrong');await delay(0);check(button('确认永久删除备份').disabled,'wrong full ID cannot delete');field('备份清理确认编号',version.id);await delay(0);submit();await taskFinished('delete');await wait(()=>d.querySelectorAll('.backup-version').length===0,'version deleted');
 check(await content('source','documents/notes.txt')===original&&await content('restored','restored-copy/notes.txt')===original,'deleting backup preserves original and restored copies');
 check(errors.length===0,'no DOM errors: '+errors.join('; '));
 console.log('Native backup DOM passed: plan/consent, bounded run, verify, restore bytes, no overwrite and exact-ID cleanup.');
}catch(error){console.error('Backup test failure:',error);throw error;}finally{await delay(100);dom.window.close();}
