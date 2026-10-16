// The Go server owns disposable anime state. Only TMDB HTTP responses are mocked
// at the browser boundary; the engine's fixed-origin client has separate tests.
import { JSDOM, VirtualConsole } from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18094',token=process.argv[3]||'';
const headers=token?{Authorization:`Bearer ${token}`}:{},errors=[],vc=new VirtualConsole();
vc.on('jsdomError',(error)=>errors.push(error.message));
const request=(path,options={})=>fetch(new URL(path,base),{...options,headers:{...options.headers,...headers}});
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#media',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
async function wait(test,label){const until=Date.now()+7000;while(Date.now()<until){if(test())return;await delay(25);}throw new Error('timed out: '+label);}
const button=(label,scope=d.querySelector('.page.active')||d)=>[...scope.querySelectorAll('button')].find((node)=>(node.textContent.trim()===label||node.getAttribute('aria-label')===label));
const card=(name)=>[...d.querySelectorAll('.page.active .subscription-card')].find((node)=>node.querySelector('h2')?.textContent===name);
function input(selector,value){const node=d.querySelector(selector);check(node,'input '+selector);node.value=value;node.dispatchEvent(new w.Event('input',{bubbles:true}));}
function select(selector,value){const node=d.querySelector(selector);check(node,'select '+selector);node.value=value;node.dispatchEvent(new w.Event('change',{bubbles:true}));}
function submit(){d.querySelector('dialog form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));}
w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
let searchCalls=0,detailCalls=0;
w.fetch=async(path,options={})=>{
  const url=new URL(path,base);
  if(url.pathname==='/api/v1/anime/settings'){const response=await request(path,options),data=await response.json();return new Response(JSON.stringify({...data,tmdb_configured:true}));}
  if(url.pathname==='/api/v1/anime/metadata/search'){
    searchCalls++;check(url.searchParams.get('q')==='Metadata fixture','metadata search query comes from draft');
    return new Response(JSON.stringify({page:1,has_more:false,truncated:false,results:[{id:42,name:'番剧 <img src=x onerror=alert(1)>',original_name:'Original',first_air_date:'2026-01-01',overview:'Metadata fixture overview'}]}));
  }
  if(url.pathname==='/api/v1/anime/metadata/series/42'){
    detailCalls++;return new Response(JSON.stringify({id:42,name:'元数据测试番剧',original_name:'Original',seasons:[{season_number:1,name:'第一季',episode_count:24},{season_number:2,name:'第二季',episode_count:12}]}));
  }
  return request(path,options);
};
const fixture='Anime management '+Date.now();
try{
  w.eval(await(await request('/app.js')).text());
  await wait(()=>d.querySelector('.page.active .anime-view')&&!d.querySelector('.page.active .loading-state'),'anime page');check(!d.querySelector('.page.active .anime-view .stat-strip')&&!button('连接与调度')&&!button('添加订阅'),'daily shelf hides administrative statistics, settings and subscription management');w.location.hash='admin/anime';await wait(()=>button('添加订阅')&&!button('添加订阅').disabled,'anime configured');check(d.querySelector('.page.active .anime-view .stat-strip'),'management view exposes operational controls');button('添加订阅').click();
  await wait(()=>d.querySelector('dialog form'),'subscription form');
  input('input[name="title"]',fixture);input('input[name="feed_url"]','https://example.com/fixture.rss');
  input('input[name="include"]','Original.*1080p');
  d.querySelector('details.advanced-fields').open=true;
  input('input[aria-label="TMDB 搜索名称"]','Metadata fixture');await delay(0);button('搜索元数据').click();
  await wait(()=>d.querySelector('.metadata-results li'),'metadata results');
  check(!d.querySelector('.metadata-results img'),'metadata title is plain text');button('选择',d.querySelector('.metadata-results')).click();
  await wait(()=>d.querySelector('.metadata-selection'),'metadata details');
  check(d.querySelector('input[name="tmdb_id"]').value==='42','selected TV ID updates draft');
  select('select[aria-label="TMDB 季数选择"]','2');await delay(0);
  check(d.querySelector('input[name="season"]').value==='2','season selector updates archive season');
  check(d.querySelector('input[name="title"]').value===fixture&&d.querySelector('input[name="include"]').value==='Original.*1080p','metadata selection preserves title and RSS rule');
  submit();await wait(()=>!d.querySelector('dialog')&&card(fixture),'subscription saved');
  let saved=(await(await request('/api/v1/anime/subscriptions')).json()).subscriptions.find((sub)=>sub.title===fixture);
  check(saved.tmdb_id===42&&saved.season===2&&saved.include==='Original.*1080p','metadata selection persists through real Go API');
  button('暂停',card(fixture)).click();await wait(()=>card(fixture)?.textContent.includes('已暂停'),'quick pause');
  button('启用',card(fixture)).click();await wait(()=>card(fixture)?.textContent.includes('追番中'),'quick enable manual subscription');
  button('归档',card(fixture)).click();await wait(()=>button('确认归档订阅'),'archive confirmation');
  check(d.querySelector('dialog').textContent.includes('观看记录'),'archive explains retained history');button('确认归档订阅').click();
  await wait(()=>!d.querySelector('dialog')&&!card(fixture),'archived subscription leaves current list');
  select('select[aria-label="订阅筛选"]','archived');await wait(()=>card(fixture)&&button('恢复订阅',card(fixture)),'archived filter');
  saved=(await(await request('/api/v1/anime/subscriptions')).json()).subscriptions.find((sub)=>sub.title===fixture);
  check(saved.archived_at>0&&!saved.enabled,'archive is durable and scheduling is off');
  button('恢复订阅',card(fixture)).click();await wait(()=>!card(fixture),'restore leaves archived list');
  select('select[aria-label="订阅筛选"]','paused');await wait(()=>card(fixture)?.textContent.includes('已暂停'),'restore remains paused');
  button('编辑',card(fixture)).click();await wait(()=>d.querySelector('dialog form'),'edit restored subscription');
  d.querySelector('input[name="auto_download"]').click();submit();await wait(()=>!d.querySelector('dialog')&&card(fixture)?.textContent.includes('自动下载'),'auto-download setting saved');
  button('启用',card(fixture)).click();await wait(()=>button('确认启用订阅'),'auto-download enable confirmation');
  check(d.querySelector('dialog').textContent.includes('历史剧集'),'auto-download prompt explains backfill');button('取消',d.querySelector('dialog')).click();
  await wait(()=>!d.querySelector('dialog'),'enable cancelled');
  saved=(await(await request('/api/v1/anime/subscriptions')).json()).subscriptions.find((sub)=>sub.title===fixture);
  check(!saved.enabled&&saved.auto_download&&saved.tmdb_id===42&&saved.season===2,'cancelled enable keeps scheduling off and metadata intact');
  check(searchCalls===1&&detailCalls===1,'explicit search and detail requests only');check(errors.length===0,'no DOM errors: '+errors.join('; '));
  console.log('Anime metadata selection and subscription management passed against disposable state.');
}finally{dom.window.close();}
