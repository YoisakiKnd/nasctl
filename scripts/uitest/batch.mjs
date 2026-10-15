// Compiled Svelte + actual nasctl, only against explicitly marked generated data.
import {JSDOM,VirtualConsole} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18103';
const requests=[];
const request=async(path,options={})=>{requests.push({path:String(path),method:options.method||'GET',body:options.body});return fetch(new URL(path,base),options);};
const marker=await request('/api/v1/files/download?root=data&path=.nasctl-ui-fixture');
if(!marker.ok||await marker.text()!=='nasctl-batch-fixture-v1\n')throw new Error('Refusing to mutate an unmarked fixture.');
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',(e)=>errors.push(e.message));
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#files',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((r)=>setTimeout(r,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
async function wait(test,label){const until=Date.now()+9000;while(Date.now()<until){if(test())return;await delay(25);}throw new Error('timed out: '+label+' '+d.querySelector('[role="alert"]')?.textContent);}
const buttons=(label)=>[...d.querySelectorAll('button')].filter((b)=>b.textContent.trim()===label);
const click=(label)=>{const b=buttons(label).at(-1);if(!b||b.disabled)throw new Error('button unavailable: '+label);b.click();};
const entry=(name)=>[...d.querySelectorAll('.finder-entry')].find((n)=>n.querySelector('.entry-label').textContent===name);
const selected=()=>d.querySelectorAll('.finder-entry[aria-selected="true"]').length;
const field=(label,value)=>{const f=d.querySelector('[aria-label="'+label+'"]');f.value=value;f.dispatchEvent(new w.Event('input',{bubbles:true}));f.dispatchEvent(new w.Event('change',{bubbles:true}));};
const selectTwo=async()=>{entry('Alpha.txt').click();entry('Beta.txt').dispatchEvent(new w.MouseEvent('click',{bubbles:true,ctrlKey:true}));await wait(()=>selected()===2,'two selections');};
const submit=()=>d.querySelector('dialog form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
const batchCalls=()=>requests.filter((r)=>r.path==='/api/v1/files/batch'&&r.method==='POST');
async function content(path){const r=await request('/api/v1/files/download?root=data&path='+encodeURIComponent(path));check(r.ok,'download '+path);return r.text();}
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
try{
 w.eval(await(await request('/app.js')).text());await wait(()=>entry('Alpha.txt')&&entry('Beta.txt'),'fixture files');
 check(d.querySelector('[role="listbox"]').getAttribute('aria-multiselectable')==='true','accessible multiselect');
 entry('Alpha.txt').click();entry('Gamma.txt').dispatchEvent(new w.MouseEvent('click',{bubbles:true,shiftKey:true}));await wait(()=>selected()===3,'Shift range selection');
 d.querySelector('[aria-label="取消选择"]').click();await wait(()=>selected()===0,'clear selection');
 click('多选');await delay(0);click('选择本页');await wait(()=>selected()===4,'select page within local cap');
 d.querySelector('[aria-label="取消选择"]').click();click('多选');await selectTwo();
 check(d.querySelector('.finder-details').textContent.includes('已选择 2 个项目'),'multi selection summary replaces single preview');
 click('批量复制');await wait(()=>d.querySelector('dialog[open]'),'batch confirmation');
 check(batchCalls().length===0,'no batch mutation before confirmation');
 check(Number(d.querySelector('[aria-label="批量条目预算"]').max)===50&&Number(d.querySelector('[aria-label="批量字节预算"]').max)===4194304,'server policy shown in form');
 field('批量目标文件夹','target');submit();
 await wait(()=>d.querySelector('.file-batch-result')?.textContent.includes('目标同名，未覆盖'),'per-item conflict result');
 check(d.querySelector('.file-batch-result').textContent.includes('完成 1 项 · 失败 1 项'),'partial success explicitly reported');
 check(await content('target/Alpha.txt')==='existing-generated-destination\n','conflicting file preserved');
 check(await content('target/Beta.txt')==='nasctl-generated-Beta.txt\n','next item copied after conflict');
 await wait(()=>entry('Alpha.txt')&&entry('Beta.txt'),'source listing');await selectTwo();click('批量回收');await wait(()=>d.querySelector('dialog[open]'),'trash confirmation');submit();
 await wait(()=>d.querySelector('.file-batch-result')?.textContent.includes('完成 2 项 · 失败 0 项'),'batch trash done');await wait(()=>!entry('Alpha.txt')&&!entry('Beta.txt'),'sources moved to trash');
 [...d.querySelectorAll('.finder-location')].find((b)=>b.textContent.includes('回收站')).click();await wait(()=>d.querySelectorAll('.finder-trash input[type="checkbox"]').length===2,'two trash entries');
 for(const box of d.querySelectorAll('.finder-trash input[type="checkbox"]'))box.click();
 await wait(()=>buttons('批量恢复').some((b)=>!b.disabled),'batch restore action');click('批量恢复');await wait(()=>d.querySelector('dialog[open]'),'restore confirmation');submit();
 await wait(()=>d.querySelector('.file-batch-result')?.textContent.includes('完成 2 项 · 失败 0 项'),'batch restore done');await wait(()=>!d.querySelector('.finder-trash'),'trash emptied');
 check(await content('Alpha.txt')==='nasctl-generated-Alpha.txt\n'&&await content('Beta.txt')==='nasctl-generated-Beta.txt\n','restored bytes match originals');
 const payloads=batchCalls().map((r)=>JSON.parse(r.body));check(payloads.length===3&&payloads.every((p)=>p.items.length===2&&p.limit===50&&p.max_bytes===4194304),'complete bounded selection submitted for each confirmed batch');
 check(errors.length===0,'no DOM errors: '+errors.join('; '));
 console.log('Batch Finder DOM passed: range/toggle/page selection, confirmation, policy, conflict continuation, trash and restore.');
}catch(error){console.error('Batch test failure:',error);throw error;}finally{await delay(100);dom.window.close();}
