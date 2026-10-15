import {JSDOM,VirtualConsole} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18101',errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const request=(path,options={})=>fetch(new URL(path,base),options);
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#media',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
async function wait(test,label){const until=Date.now()+10000;while(Date.now()<until){if(test())return;await delay(25);}throw new Error('timed out: '+label);}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find((node)=>node.textContent.trim()===label);
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
const download=(path)=>request('/api/v1/files/download?'+new URLSearchParams({root:'library',path:path.replace(/^\//,'')}));
try{
 w.eval(await(await request('/app.js')).text());await wait(()=>[...d.querySelectorAll('.subscription-card')].some((node)=>node.textContent.includes('Generated Metadata Fixture')),'generated subscription');
 const card=[...d.querySelectorAll('.subscription-card')].find((node)=>node.textContent.includes('Generated Metadata Fixture'));button('更新元数据',card).click();await wait(()=>d.querySelector('dialog'),'metadata options');
 const limit=d.querySelector('[aria-label="元数据处理限额"]');limit.value='1';limit.dispatchEvent(new w.Event('input',{bubbles:true}));
 [...d.querySelectorAll('dialog label')].find((node)=>node.textContent.includes('替换已有 NFO')).querySelector('input').click();await delay(0);button('确认更新元数据',d.querySelector('dialog')).click();
 await wait(()=>button('继续下一批元数据')&&!button('继续下一批元数据').disabled,'first bounded metadata batch completed');
 button('继续下一批元数据').click();await delay(0);await wait(()=>d.querySelector('.task-panel .task-monitor .badge')?.textContent==='已完成'&&!button('继续下一批元数据'),'second batch completed');
 button('连接与调度').click();await delay(0);button('查看元数据备份').click();await wait(()=>d.querySelector('.metadata-backups tbody tr'),'previous metadata versions');
 const row=[...d.querySelectorAll('.metadata-backups tbody tr')].find((node)=>node.querySelector('td').textContent==='Generated Metadata Fixture/tvshow.nfo');check(row,'old series metadata is listed');
 const response=await request(row.querySelector('a[download]').getAttribute('href'));check(response.ok&&response.headers.get('content-disposition').startsWith('attachment;')&&(await response.text()).includes('fixture custom series metadata'),'previous NFO export preserves original bytes');
 button('恢复',row).click();await wait(()=>d.querySelector('dialog'),'restore confirmation');button('确认恢复元数据',d.querySelector('dialog')).click();
 await wait(()=>!d.querySelector('dialog')&&d.querySelector('.metadata-backups .task-monitor .badge')?.textContent==='已完成','restore task completed');
 const restored=await download('/Generated Metadata Fixture/tvshow.nfo');check(restored.ok&&(await restored.text())==='<tvshow>fixture custom series metadata</tvshow>','restored file bytes match previous version');
 const video=await download('/Generated Metadata Fixture/Season 1/Generated Metadata Fixture S01E01.mp4');check(video.ok&&(await video.text())==='generated original video bytes','original video remains unchanged');
 const subtitle=await download('/Generated Metadata Fixture/Season 1/Generated Metadata Fixture S01E01.zh.srt');check(subtitle.ok&&(await subtitle.text())==='generated original subtitle','original subtitle remains unchanged');
 check(errors.length===0,'no DOM errors: '+errors.join('; '));console.log('Bounded metadata UI update, continuation, backup export and real byte restore passed.');
}finally{dom.window.close();}
