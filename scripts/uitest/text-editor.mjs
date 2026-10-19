// Real Svelte + real text view/edit APIs on a generated, writable fixture root.
import {JSDOM,VirtualConsole} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18090',token=process.argv[3]||'',headers=token?{Authorization:'Bearer '+token}:{};
const request=(path,options={})=>fetch(new URL(path,base),{...options,headers:{...options.headers,...headers}});
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#files',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
async function wait(test,label){const until=Date.now()+7000;while(Date.now()<until){if(test())return;await delay(25);}throw new Error('timed out: '+label+' '+(d.querySelector('[role=alert]')?.textContent||''));}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find((node)=>node.getAttribute('aria-label')===label||node.textContent.trim()===label);
const option=(name)=>[...d.querySelectorAll('.finder-entry')].find((node)=>node.querySelector('.entry-label').textContent===name);
const type=async(node,value)=>{node.value=value;node.dispatchEvent(new w.Event('input',{bubbles:true}));await delay(20);};
const name='generated-note-'+Date.now().toString(36)+'.md';
let root='';const text=async()=>(await(await request('/api/v1/files/text?root='+root+'&path='+encodeURIComponent(name))).json()).text;
w.fetch=request;w.AbortController=AbortController;w.Blob=Blob;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});w.confirm=()=>true;
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
try{
 w.eval(await(await request('/app.js')).text());await wait(()=>button('新建文件')&&!button('新建文件').disabled,'new file action');
 button('新建文件').click();await wait(()=>d.querySelector('dialog [aria-label="文件操作目标"]'),'new file name field');
 await type(d.querySelector('dialog [aria-label="文件操作目标"]'),name);button('确认新建文件').click();
 await wait(()=>d.querySelector('textarea[aria-label="编辑文本"]'),'a new file opens in the editor');
 // The page opens on the first writable root; find which one that is.
 for(const item of (await(await request('/api/v1/files/roots')).json()).roots){if((await request('/api/v1/files/stat?root='+item.id+'&path='+encodeURIComponent(name))).ok)root=item.id;}
 check(root&&(await text()).content===''&&(await text()).revision,'new file is created empty and editable');
 const editor=d.querySelector('textarea[aria-label="编辑文本"]');await type(editor,'# 生成的笔记\n第一行\n');
 check(d.querySelector('.text-file footer').textContent.includes('未保存'),'unsaved changes are marked');
 editor.dispatchEvent(new w.KeyboardEvent('keydown',{key:'s',ctrlKey:true,bubbles:true,cancelable:true}));
 await wait(()=>d.querySelector('pre[aria-label="文本内容"]')?.textContent==='# 生成的笔记\n第一行\n','Ctrl+S saves and returns to the view');
 check((await text()).content==='# 生成的笔记\n第一行\n','saved bytes reach the file');
 // Another client saves first; this editor's revision is now stale.
 const current=await text();
 const other=await request('/api/v1/files/text?root='+root+'&path='+encodeURIComponent(name)+'&revision='+current.revision,{method:'PUT',headers:{'X-Nasctl-Confirm':'1'},body:'changed elsewhere\n'});
 check(other.ok,'a second client saves');
 button('编辑').click();await wait(()=>d.querySelector('textarea[aria-label="编辑文本"]'),'editing again');
 await type(d.querySelector('textarea[aria-label="编辑文本"]'),'stale edit\n');button('保存').click();
 await wait(()=>d.querySelector('.text-file [role=alert]')?.textContent.includes('被修改过'),'a stale save is refused with an explanation');
 check((await text()).content==='changed elsewhere\n','the newer content is kept');
 button('取消').click();await delay(0);button('关闭').click();await wait(()=>!d.querySelector('dialog'),'editor closes');
 option(name).click();await wait(()=>d.querySelector('.text-excerpt pre')?.textContent==='changed elsewhere\n','details pane shows a text excerpt');
 check(!d.querySelector('.text-excerpt script,.text-file script'),'text is shown as text, never markup');
 const removed=await request('/api/v1/files/trash',{method:'POST',headers:{'Content-Type':'application/json','X-Nasctl-Confirm':'1'},body:JSON.stringify({root,path:name})});
 check(removed.ok,'generated note moved to the trash');
 check(errors.length===0,'no DOM errors: '+errors.join('; '));
 console.log('Text view, editing, conflict refusal and new files passed.');
}catch(failure){console.error("FAIL:",failure.message);process.exitCode=1;await delay(100);}finally{dom.window.close();}
