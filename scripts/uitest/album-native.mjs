// Actual nasctl -> on-demand `nasctl album` pause/resume, plus local directory UI.
// Uses generated disposable fixture data; does not claim decoder/cgroup tests.
import { JSDOM, VirtualConsole } from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18099';
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const request=(path,options={})=>fetch(new URL(path,base),options);
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#album',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
async function wait(test,label){const until=Date.now()+12000;while(Date.now()<until){if(test())return;await delay(30);}throw new Error('timed out: '+label);}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find((node)=>(node.getAttribute('aria-label')||node.textContent.trim())===label);
function input(label,value){const node=d.querySelector(`[aria-label="${label}"]`);check(node,'input '+label);node.value=value;node.dispatchEvent(new w.Event(node.tagName==='SELECT'?'change':'input',{bubbles:true}));}
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
const admin=()=>d.querySelector('.page[data-page="admin/album"]');
async function manage(){button('相册更多操作').click();await delay(0);d.querySelector('.photo-menu a[href="#admin/album"]').click();w.location.hash='admin/album';await wait(()=>admin()&&!admin().hidden&&admin().querySelector('.album-directory-form')&&button('确认限量扫描',admin()),'album management page');}
try{
 const state=await(await request('/api/v1/album/status')).json();check(state.mode==='local'&&state.connected&&state.index_ready,'native reader uses the existing local index');
 w.eval(await(await request('/app.js')).text());await wait(()=>d.querySelectorAll('.album-tile').length===48,'native gallery');
 await manage();check(admin().textContent.includes('主配置文件'),'main configuration collections remain explicit');
 input('相册集合代码','extra');input('相册目录名称','额外照片');input('相册存储位置','photos');input('相册相对目录','Additional');
 [...admin().querySelectorAll('label')].find((node)=>node.textContent.includes('允许照片与系统')).querySelector('input').click();await delay(0);
 check(d.querySelector('form.album-directory-form').checkValidity(),'album directory native form validity');
 d.querySelector('form.album-directory-form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
 await delay(0);
 await wait(()=>[...d.querySelectorAll('.album-directory-row')].some((node)=>node.textContent.includes('额外照片'))&&button('确认添加目录')&&!button('确认添加目录').disabled,'directory saved through real API');
 let settings=await(await request('/api/v1/album/settings')).json();check(settings.directories.some((item)=>item.code==='extra'&&item.path==='Additional'&&item.allow_shared_volume),'root-relative directory persisted');
 const row=[...d.querySelectorAll('.album-directory-row')].find((node)=>node.textContent.includes('额外照片'));button('编辑',row).click();await delay(0);
 input('相册目录名称','额外照片（停用）');[...admin().querySelectorAll('label')].find((node)=>node.textContent.includes('启用此集合')).querySelector('input').click();await delay(0);
 button('确认保存目录').click();await delay(0);await wait(()=>button('确认添加目录')&&button('确认添加目录')&&!button('确认添加目录').disabled,'directory edit saved');
 settings=await(await request('/api/v1/album/settings')).json();check(settings.directories.some((item)=>item.code==='extra'&&!item.enabled),'enabled state saved');
 button('暂停后续处理',admin()).click();await wait(()=>admin().querySelector('.task-monitor'),'pause task accepted');
 await wait(()=>admin().textContent.includes('相册处理已暂停')&&button('恢复处理许可',admin())&&!button('恢复处理许可',admin()).disabled,'actual nasctl album pause is visible through local reader');
 w.location.hash='album';await wait(()=>d.querySelector('.album-view .photo-status')?.textContent.includes('相册处理已暂停'),'the gallery refreshes and shows the pause');
 await manage();button('恢复处理许可',admin()).click();
 await wait(()=>admin().querySelector('.task-monitor .badge')?.textContent==='已完成'&&!admin().textContent.includes('相册处理已暂停'),'actual nasctl album resume completes');
const savedRow=[...d.querySelectorAll('.album-directory-row')].find((node)=>node.textContent.includes('额外照片（停用）'));button('移除',savedRow).click();await delay(0);button('确认移除目录配置').click();
 await wait(()=>d.querySelectorAll('.album-directory-row').length===1&&button('确认添加目录')&&!button('确认添加目录').disabled,'managed directory removed without touching originals');
 const response=await request('/api/v1/album/tasks/scan',{method:'POST',headers:{'Content-Type':'application/json','X-Nasctl-Confirm':'1'},body:JSON.stringify({collection:'photos',limit:0,max_runtime:'10m'})});check(response.status===400,'uncapped worker cannot start');await response.arrayBuffer();
 check(errors.length===0,'no DOM errors: '+errors.join('; '));
 console.log('Native album configuration and real isolated pause/resume workflow passed.');
}finally{dom.window.close();}
