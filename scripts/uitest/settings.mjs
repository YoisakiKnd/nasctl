// Real compiled Svelte and real HTTP settings. Generated fixture marker required.
import {JSDOM,VirtualConsole} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18105';
const check=(ok,label)=>{if(!ok)throw Error(label);console.log('ok:',label);};
const pause=ms=>new Promise(r=>setTimeout(r,ms));
async function wait(fn,label){const end=Date.now()+10000;while(Date.now()<end){if(fn())return;await pause(25)}throw Error('timeout: '+label)}
const initial=await(await fetch(base+'/api/v1/settings')).json();
check(initial.saved.storage.length===1&&initial.saved.storage[0].label==='Generated files','fresh generated settings fixture');
const marker=await(await fetch(base+'/api/v1/files/download?root=data&path=.nasctl-ui-fixture')).text();check(marker==='nasctl-settings-fixture-v1\n','fixture ownership marker');
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',e=>errors.push(e.message));
const dom=new JSDOM(await(await fetch(base+'/')).text(),{url:base+'/#settings',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});const w=dom.window,d=w.document;
w.fetch=(p,o={})=>fetch(new URL(p,base),o);w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','')};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');this.dispatchEvent(new w.Event('close'))};
const button=label=>[...d.querySelectorAll('button')].find(n=>n.textContent.trim()===label);
const input=(label,value)=>{const n=d.querySelector('input[aria-label="'+label+'"]');check(n,'input '+label);n.value=value;n.dispatchEvent(new w.Event('input',{bubbles:true}))};
try{
 w.eval(await(await fetch(base+'/app.js')).text());await wait(()=>d.querySelector('.nas-settings fieldset')&&!d.querySelector('.nas-settings fieldset').disabled,'settings resolves');
 check(!d.body.textContent.includes('fixture-initial-private'),'initial secrets not rendered');
 w.location.hash='home';await wait(()=>d.querySelector('.storage-meter-list small'),'home storage card');const cap=d.querySelector('.storage-meter-list small')?.textContent||'';check(cap.includes('可用')&&!cap.includes('暂不可用'),'real filesystem capacity rendered');w.location.hash='settings';await wait(()=>d.querySelector('.page.active .nas-settings'),'back to system settings');
 input('上传上限（字节）','2048');
 button('校验配置').click();await wait(()=>d.querySelector('.nas-settings [role="status"]')?.textContent.includes('校验通过'),'validation feedback');
 check(!(await(await fetch(base+'/api/v1/settings')).json()).restart_required,'validation does not save');
 input('上传上限（字节）','4096');await pause(50);check(button('保存配置…').disabled,'editing invalidates checked consent');
 const secret=d.querySelector('select[aria-label="下载器密码操作"]');secret.value='set';secret.dispatchEvent(new w.Event('change',{bubbles:true}));await wait(()=>d.querySelector('input[aria-label="新下载器密码"]'),'new secret input');input('新下载器密码','fixture-new-private-password');
 input('存储目录名称 1','Saved generated files');button('校验配置').click();await wait(()=>button('保存配置…')&&!button('保存配置…').disabled,'revalidation');
 button('保存配置…').click();await wait(()=>d.querySelector('dialog[open]'),'explicit save confirmation');check(d.querySelector('dialog').textContent.includes('重启'),'restart warning in consent');
 button('确认保存，下次启动生效').click();await wait(()=>!d.querySelector('dialog')&&d.querySelector('.nas-settings [role="status"]')?.textContent.includes('配置已保存'),'save succeeds');
 const saved=await(await fetch(base+'/api/v1/settings')).json();check(saved.restart_required&&saved.saved.files.upload_max_bytes===4096&&saved.active.files.upload_max_bytes!==4096,'saved and current runtime distinct');check(saved.saved.storage[0].label==='Saved generated files'&&saved.active.storage[0].label==='Generated files','new location label awaits restart');
 check(!JSON.stringify(saved).includes('fixture-new-private-password')&&!d.body.textContent.includes('fixture-new-private-password'),'new secrets not returned or echoed');check(!d.querySelector('input[aria-label="新下载器密码"]'),'new secret cleared after save');
 const file=await(await fetch(base+'/api/v1/files/download?root=data&path=generated.txt')).text();check(file==='original settings fixture bytes\n','original fixture file unchanged');check(!errors.length,'no Svelte DOM errors: '+errors.join(';'));
 console.log('Unified settings DOM workflow passed');
}finally{dom.window.close()}
