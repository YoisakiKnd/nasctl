// Exercise the compiled Svelte file manager against disposable writable storage.
import { JSDOM, VirtualConsole } from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18093',token=process.argv[3]||'';
const headers=token?{Authorization:`Bearer ${token}`}:{}, errors=[];
const vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const dom=new JSDOM(await(await fetch(base+'/',{headers})).text(),{url:base+'/#files',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document;
const delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
async function wait(test,label){const until=Date.now()+7000;while(Date.now()<until){if(test())return;await delay(25);}throw new Error('timed out: '+label);}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find((item)=>item.textContent.trim()===label);
const row=(name)=>[...d.querySelectorAll('.finder-entry,tbody tr')].find((item)=>item.textContent.includes(name));
const request=(path,init={})=>fetch(new URL(path,base),{...init,headers:{...init.headers,...headers}});
const input=(selector,value)=>{const node=d.querySelector(selector);check(node,'input '+selector);node.value=value;node.dispatchEvent(new w.Event('input',{bubbles:true}));};
const submit=()=>d.querySelector('dialog form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
const NativeXHR=w.XMLHttpRequest;
w.XMLHttpRequest=class extends NativeXHR{send(body){if(token)this.setRequestHeader('Authorization',`Bearer ${token}`);super.send(body);}};
w.fetch=request;w.AbortController=AbortController;
w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};
w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
async function chooseUpload(name,content){
  d.querySelector('button[aria-label="取消选择"]')?.click();await delay(0);
  button('上传文件').click();await wait(()=>d.querySelector('dialog input[type=file]'),'upload dialog');
  const picker=d.querySelector('input[type=file]');Object.defineProperty(picker,'files',{value:[new w.File([content],name,{type:'text/plain'})]});picker.dispatchEvent(new w.Event('change',{bubbles:true}));await delay(0);submit();
}
async function downloadLink(name){row(name).click();await delay(0);await wait(()=>d.querySelector('.finder-details a[download]'),'selected file download');return d.querySelector('.finder-details a[download]').getAttribute('href');}
async function operation(name,label){
  row(name).dispatchEvent(new w.MouseEvent('contextmenu',{bubbles:true,cancelable:true,clientX:100,clientY:100}));
  await wait(()=>d.querySelector('.file-context-menu'),'file menu');
  await delay(0);button(label,d.querySelector('.file-context-menu')).click();
}
try{
  w.eval(await(await request('/app.js')).text());
  await wait(()=>button('新建文件夹')&&!button('新建文件夹').disabled,'writable controls');
  const fixture='DOM Files '+Date.now();button('新建文件夹').click();
  await wait(()=>d.querySelector('dialog'),'create dialog');input('input[aria-label="文件操作目标"]',fixture);submit();
  await wait(()=>!d.querySelector('dialog')&&row(fixture),'directory is created');
  row(fixture).dispatchEvent(new w.MouseEvent('dblclick',{bubbles:true}));await wait(()=>d.querySelectorAll('.breadcrumbs button').length===2&&!d.querySelector('.loading-state'),'enter directory');
  await chooseUpload('说明.txt','fixture upload content');
  await wait(()=>!d.querySelector('dialog')&&row('说明.txt'),'XHR File upload completes');
  let download=await downloadLink('说明.txt');
  check(await(await request(download)).text()==='fixture upload content','uploaded bytes match the File');
  await operation('说明.txt','改名／移动');await wait(()=>d.querySelector('dialog'),'rename dialog');
  input('input[aria-label="文件操作目标"]',fixture+'/改名.txt');submit();
  await wait(()=>!d.querySelector('dialog')&&row('改名.txt'),'rename updates listing');
  await operation('改名.txt','移入回收站');await wait(()=>d.querySelector('dialog'),'trash review');submit();
  await wait(()=>!d.querySelector('dialog')&&!row('改名.txt'),'entry moves into trash');
  button('回收站').click();await wait(()=>row('改名.txt')&&button('恢复',row('改名.txt')),'trash listing');
  button('恢复',row('改名.txt')).click();await wait(()=>d.querySelector('dialog'),'restore review');submit();
  await wait(()=>!d.querySelector('dialog')&&!row('改名.txt'),'restored entry leaves trash');
  d.querySelector('button[aria-label="后退"]').click();await wait(()=>row('改名.txt'),'restored entry returns to original directory');
  download=await downloadLink('改名.txt');
  check(await(await request(download)).text()==='fixture upload content','trash and restore preserve bytes');
  await chooseUpload('改名.txt','should never overwrite');
  await wait(()=>d.querySelector('dialog [role="alert"]')?.textContent.includes('已存在'),'upload conflict is shown');
  check(await(await request(download)).text()==='fixture upload content','same-name conflict preserves old content');
  d.querySelector('dialog button[aria-label="关闭对话框"]').click();await wait(()=>!d.querySelector('dialog'),'failed upload can be dismissed');
  await operation('改名.txt','复制');await wait(()=>d.querySelector('dialog'),'copy review');
  input('input[aria-label="文件操作目标"]',fixture+'/副本.txt');submit();
  await wait(()=>!d.querySelector('dialog')&&row('副本.txt'),'background copy completes and refreshes');
  const copiedDownload=await downloadLink('副本.txt');
  check(await(await request(copiedDownload)).text()==='fixture upload content','background copy preserves content');
  check(await(await request(download)).text()==='fixture upload content','copy preserves source');
  await operation('副本.txt','移动');await wait(()=>d.querySelector('dialog'),'move review');
  input('input[aria-label="文件操作目标"]',fixture+'/移动.txt');submit();
  await wait(()=>!d.querySelector('dialog')&&row('移动.txt')&&!row('副本.txt'),'background move updates listing');
  await operation('移动.txt','移入回收站');await wait(()=>d.querySelector('dialog'),'trash copied fixture');submit();
  await wait(()=>!d.querySelector('dialog')&&!row('移动.txt'),'copied fixture trashed');
  button('回收站').click();await wait(()=>row('移动.txt'),'purge fixture available');
  button('永久删除',row('移动.txt')).click();await wait(()=>d.querySelector('dialog'),'permanent deletion review');
  check(button('确认永久删除').disabled,'purge requires exact item confirmation');
  input('input[aria-label="永久删除确认名称"]','wrong');await delay(0);check(button('确认永久删除').disabled,'wrong name cannot confirm purge');
  input('input[aria-label="永久删除确认名称"]','移动.txt');await delay(0);check(!button('确认永久删除').disabled,'exact fixture name enables purge');submit();
  await wait(()=>!d.querySelector('dialog')&&!row('移动.txt'),'confirmed fixture purge completes');
  check(await(await request(download)).text()==='fixture upload content','purge affects only the copied fixture');
  check(errors.length===0,'no DOM errors: '+errors.join('; '));
  console.log('Writable file management flows passed against disposable storage.');
}finally{dom.window.close();}
