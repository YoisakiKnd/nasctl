import {JSDOM,VirtualConsole} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18101',fixture=process.argv[3]||'http://127.0.0.1:18102';
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const request=(path,options={})=>fetch(new URL(path,base),options);
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#media',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
async function wait(test,label){const until=Date.now()+8000;while(Date.now()<until){if(test())return;await delay(25);}throw new Error('timed out: '+label);}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find((node)=>node.textContent.trim()===label);
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
try{
 w.eval(await(await request('/app.js')).text());await wait(()=>button('剧集与媒体库'),'anime app loaded');button('剧集与媒体库').click();
 await wait(()=>button('暂停下载'),'episode control visible');
 const before=await(await fetch(fixture+'/__test/stats')).json();
 for(const [label,state] of [['暂停下载','下载已暂停'],['恢复下载','下载中'],['重新校验','校验中']]){
  button(label).click();await wait(()=>d.querySelector('dialog'),'control confirmation');
  const awaiting=await(await fetch(fixture+'/__test/stats')).json();check(awaiting.controls.length===before.controls.length+(label==='暂停下载'?0:label==='恢复下载'?1:2),'no side effect before confirmation '+label);
  button('确认'+label,d.querySelector('dialog')).click();await delay(0);
  await wait(()=>!d.querySelector('dialog')&&[...d.querySelectorAll('.episode-panel tbody tr')].find((node)=>node.querySelector('.episode-name strong')?.textContent==='Generated Control Fixture')?.querySelector('.badge')?.textContent===state,'confirmed '+label);
 }
 button('连接与调度').click();await delay(0);check(d.querySelector('.anime-settings-body').closest('.panel').parentNode.textContent.includes('/downloads/anime'),'container mapping is visible');
 const after=await(await fetch(fixture+'/__test/stats')).json();check(after.adds===0&&after.controls.slice(-3).join(',')==='stop,start,recheck','only scoped control calls, no download resubmission');
 check(errors.length===0,'no DOM errors: '+errors.join('; '));console.log('Real nasctl DOM to generated downloader control protocol passed.');
}finally{dom.window.close();}
