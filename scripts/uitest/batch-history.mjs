// Read-only compiled-DOM check of previously saved batch outcomes after restart.
import {JSDOM} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18103';
const request=(path,options)=>fetch(new URL(path,base),options);
const marker=await(await request('/api/v1/files/download?root=data&path=.nasctl-ui-fixture')).text();
if(marker!=='nasctl-batch-fixture-v1\n')throw Error('Expected generated fixture');
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#jobs',runScripts:'outside-only',pretendToBeVisual:true});
const w=dom.window,d=w.document,delay=()=>new Promise(r=>setTimeout(r,25));
async function wait(test,label){const end=Date.now()+9000;while(Date.now()<end){if(test())return;await delay();}throw Error(label);}
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
try{
 w.eval(await(await request('/app.js')).text());await wait(()=>d.querySelector('[aria-label="任务状态"]'),'jobs view');
 const filter=d.querySelector('[aria-label="任务状态"]');filter.value='failed';filter.dispatchEvent(new w.Event('change',{bubbles:true}));
 await wait(()=>d.querySelector('tbody tr [data-op="files.batch"]'),'saved failed batch');
 const row=d.querySelector('tbody tr [data-op="files.batch"]').closest('tr');row.querySelector('button').click();
 await wait(()=>d.querySelector('dialog .file-batch-result')?.textContent.includes('目标同名，未覆盖'),'human per-item results');
 if(!d.querySelector('dialog .file-batch-result').textContent.includes('完成 1 项 · 失败 1 项'))throw Error('partial outcome missing');
 console.log('Reopened task history shows durable per-item batch results after service restart.');
}finally{await delay();dom.window.close();}
