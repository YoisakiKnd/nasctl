import {JSDOM,VirtualConsole} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18101',errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const request=(path,options={})=>fetch(new URL(path,base),options);
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#media',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
async function wait(test,label){const until=Date.now()+10000;while(Date.now()<until){if(test())return;await delay(25);}throw new Error('timed out: '+label);}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find((node)=>node.textContent.trim()===label);
const card=()=>[...d.querySelectorAll('.subscription-card')].find((node)=>node.textContent.includes('Generated Metadata Fixture'));
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
function input(label,value){const node=d.querySelector(`[aria-label="${label}"]`);node.value=value;node.dispatchEvent(new w.Event('input',{bubbles:true}));}
async function openCleanup(){button('清理历史',card()).click();await wait(()=>d.querySelector('[aria-label="确认历史清理订阅名称"]'),'cleanup range loaded');}
try{
 w.eval(await(await request('/app.js')).text());await wait(()=>card(),'history fixture');
 const exported=await request(card().querySelector('a[download]').getAttribute('href'));check(exported.ok&&exported.headers.get('content-disposition').startsWith('attachment;'),'history export attachment');
 const lines=(await exported.text()).trim().split('\n').map((line)=>JSON.parse(line));check(lines[0].format==='nasctl-anime-history'&&lines.at(-1).complete&&lines.at(-1).episodes===2,'consistent complete history contains both archived episodes');
 button('归档',card()).click();await wait(()=>d.querySelector('dialog'),'archive confirmation');button('确认归档订阅',d.querySelector('dialog')).click();
 await wait(()=>!d.querySelector('dialog')&&!card(),'archived subscription left active view');
 const filter=d.querySelector('[aria-label="订阅筛选"]');filter.value='archived';filter.dispatchEvent(new w.Event('change',{bubbles:true}));await wait(()=>card()&&button('清理历史',card()),'archived history actions');
 await openCleanup();check(button('确认清理历史').disabled,'permanent cleanup requires exact subscription title');input('历史清理限额','1');input('确认历史清理订阅名称','wrong title');await delay(0);check(button('确认清理历史').disabled,'wrong title cannot clear history');
 input('确认历史清理订阅名称','Generated Metadata Fixture');await delay(0);button('确认清理历史').click();
 await wait(()=>!d.querySelector('dialog')&&button('继续清理下一批')&&!button('继续清理下一批').disabled,'first limited cleanup completed');
 let status=await(await request('/api/v1/anime/subscriptions/2/history')).json();check(status.episodes===1&&status.dedup===1,'one history row removed, one dedup marker retained');
 button('继续清理下一批').click();await delay(0);await wait(()=>d.querySelector('.task-panel .task-monitor .badge')?.textContent==='已完成'&&!button('继续清理下一批'),'second cleanup completed');
 status=await(await request('/api/v1/anime/subscriptions/2/history')).json();check(status.episodes===0&&status.dedup===2,'both historical releases remain deduplicated');
 await openCleanup();[...d.querySelectorAll('dialog label')].find((node)=>node.textContent.includes('同时永久移除整个订阅')).querySelector('input').click();input('确认历史清理订阅名称','Generated Metadata Fixture');await delay(0);button('确认移除订阅及历史').click();
 await wait(()=>!d.querySelector('dialog')&&!card()&&d.querySelector('.task-panel .task-monitor .badge')?.textContent==='已完成','empty archived subscription permanently removed');
 const video=await request('/api/v1/files/download?'+new URLSearchParams({root:'library',path:'Generated Metadata Fixture/Season 1/Generated Metadata Fixture S01E01.mp4'}));check(video.ok&&(await video.text())==='generated original video bytes','history removal preserves actual video bytes');
 check(errors.length===0,'no DOM errors: '+errors.join('; '));console.log('History export, exact-title cleanup, bounded continuation, dedup retention and subscription removal passed.');
}finally{dom.window.close();}
