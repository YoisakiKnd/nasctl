// Full browser-DOM -> nasctl proxy -> real albumctl service workflow. Both
// services use generated fixtures; no scan or thumbnail job is started here.
import { JSDOM, VirtualConsole } from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18096',token=process.argv[3]||'';
const headers=token?{Authorization:`Bearer ${token}`}:{},errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const request=(path,options={})=>fetch(new URL(path,base),{...options,headers:{...options.headers,...headers}});
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#album',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
async function wait(test,label){const until=Date.now()+8000;while(Date.now()<until){if(test())return;await delay(25);}throw new Error('timed out: '+label);}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find((node)=>(node.getAttribute('aria-label')||node.textContent.trim())===label);
const tiles=()=>d.querySelectorAll('.album-tile');
function input(selector,value){const node=d.querySelector(selector);check(node,'input '+selector);node.value=value;node.dispatchEvent(new w.Event('input',{bubbles:true}));}
async function apply(){d.querySelector('.album-filters').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));await delay(0);}
function close(){button('关闭对话框',d.querySelector('dialog')).click();}
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
try{
  const initialJobs=(await(await request('/api/v1/jobs?limit=200')).json()).jobs.map((job)=>job.id);
  const state=await(await request('/api/v1/album/status')).json();check(state.configured&&state.connected,'real albumctl connection works with backend-only credential');
  check(!JSON.stringify(state).includes('nasctl-disposable-album-token'),'album credential is absent from status');
  w.eval(await(await request('/app.js')).text());await wait(()=>tiles().length===48&&!d.querySelector('.loading-state'),'48-item gallery page');
  check(!d.querySelector('.photo-filter-options')&&!d.querySelector('.album-tile-info'),'photos lead the library; advanced filters and file metadata stay out of the grid');
  check(d.querySelectorAll('.photo-month').length===2,'photos are grouped by capture month');
  button('相簿').click();await wait(()=>d.querySelector('.photo-album'),'album navigation');
  check(d.querySelector('.photo-album strong').textContent==='Fixture Photos','configured photo folders appear as albums');
  d.querySelector('.photo-album').click();await wait(()=>tiles().length===48&&!d.querySelector('.loading-state'),'album opens its photo library');
  tiles()[0].click();await wait(()=>d.querySelector('dialog .album-detail'),'first lightbox');
  check(!d.querySelector('.photo-info')&&button('上一张').disabled,'viewer opens with information folded and bounded navigation');
  const first=d.querySelector('dialog .album-detail-preview img,dialog .album-detail-preview video').getAttribute('src');
  d.querySelector('dialog').dispatchEvent(new w.KeyboardEvent('keydown',{key:'ArrowRight',bubbles:true}));
  await wait(()=>d.querySelector('dialog .album-detail-preview img,dialog .album-detail-preview video')?.getAttribute('src')&&d.querySelector('dialog .album-detail-preview img,dialog .album-detail-preview video').getAttribute('src')!==first,'keyboard opens next photo');
  button('上一张').click();await wait(()=>d.querySelector('dialog .album-detail-preview img,dialog .album-detail-preview video')?.getAttribute('src')===first,'previous photo returns');
  button('照片信息').click();await wait(()=>d.querySelector('.photo-info'),'information opens on demand');
  close();await wait(()=>!d.querySelector('dialog'),'viewer closes');
  check([...d.querySelectorAll('.album-frame img')].every((image)=>/^\/api\/v1\/album\/media\/\d+\/thumb$/.test(image.getAttribute('src'))),'all thumbnail URLs use the console origin');
  const thumb=await request(d.querySelector('.album-frame img').getAttribute('src'));check(thumb.ok&&thumb.headers.get('content-type').startsWith('image/'),'real cached/placeholder thumbnail is proxied');await thumb.arrayBuffer();
  // Editing an unapplied filter must not combine it with an existing cursor.
  input('input[aria-label="搜索相册"]','<img');await delay(0);button('下一页').click();
  await wait(()=>tiles().length===8&&!d.querySelector('.loading-state'),'pagination uses applied filters, not the unsaved search');
  button('上一页').click();await wait(()=>tiles().length===48&&!d.querySelector('.loading-state'),'previous cursor restores gallery');
  await apply();await wait(()=>tiles().length===1&&!d.querySelector('.loading-state'),'filename search reaches real album index');
  check(tiles()[0].getAttribute('aria-label').includes('<img')&&!tiles()[0].querySelector('img[src=x]'),'hostile indexed filename stays plain text');
  tiles()[0].click();await wait(()=>d.querySelector('dialog .album-detail'),'media details');
  check(!d.querySelector('.photo-info'),'new viewer starts without metadata');
  const raw=d.querySelector('dialog .album-detail-preview img').getAttribute('src');check(raw.endsWith('/raw'),'only the explicitly opened photo loads its original');
  const range=await request(raw,{headers:{Range:'bytes=0-15'}}),rangeBody=new Uint8Array(await range.arrayBuffer());
  check(range.status===206&&rangeBody.length===16&&rangeBody[0]===255&&rangeBody[1]===216,'original JPEG range preserves bytes and status');
  const download=d.querySelector('dialog a[download]').getAttribute('href'),response=await request(download);check(response.ok&&response.headers.get('content-disposition').startsWith('attachment;'),'original download is a separate attachment');await response.arrayBuffer();
  close();await wait(()=>!d.querySelector('dialog'),'detail closes');
  input('input[aria-label="搜索相册"]','Missing-photo');await apply();await wait(()=>!d.querySelector('.loading-state')&&tiles().length===0,'missing originals are hidden by default');
  button('筛选').click();await delay(0);
  [...d.querySelectorAll('.album-filters label')].find((label)=>label.textContent.includes('显示缺失原文件')).querySelector('input').click();await apply();
  await wait(()=>tiles().length===1&&!d.querySelector('.loading-state'),'missing original filter retains indexed record');tiles()[0].click();await wait(()=>d.querySelector('dialog .album-detail'),'missing item detail');
  check(d.querySelector('dialog').textContent.includes('原文件暂不可用')&&!d.querySelector('dialog a[download]'),'missing original keeps thumbnail and hides unusable download');close();await wait(()=>!d.querySelector('dialog'),'missing detail closes');
  input('input[aria-label="搜索相册"]','');const kind=d.querySelector('select[aria-label="相册类型"]');kind.value='video';kind.dispatchEvent(new w.Event('change',{bubbles:true}));await apply();
  await wait(()=>tiles().length===1&&!d.querySelector('.loading-state'),'video filter');tiles()[0].click();await wait(()=>d.querySelector('dialog video'),'video uses native browser controls');check(d.querySelector('dialog video').getAttribute('src').endsWith('/raw'),'video streams through the authenticated proxy');close();
  const finalJobs=(await(await request('/api/v1/jobs?limit=200')).json()).jobs.map((job)=>job.id);check(JSON.stringify(initialJobs)===JSON.stringify(finalJobs),'album browsing creates no background jobs');
  check(errors.length===0,'no DOM errors: '+errors.join('; '));console.log('Integrated album flows passed through the real read-only albumctl service. Codec playback is a separate check.');
}finally{dom.window.close();}
