// Real Svelte + real ZIP extraction as a background task on a writable fixture root.
import {JSDOM,VirtualConsole} from 'jsdom';
import {crc32} from 'node:zlib';
const base=process.argv[2]||'http://127.0.0.1:18090',token=process.argv[3]||'',headers=token?{Authorization:'Bearer '+token}:{};
const request=(path,options={})=>fetch(new URL(path,base),{...options,headers:{...options.headers,...headers}});
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{if(!ok)throw new Error(label);console.log('ok:',label);};
// A stored (uncompressed) ZIP with UTF-8 names, built by hand: no dependency.
function zip(entries){
  const parts=[],central=[];let offset=0;
  for(const [name,text] of entries){
    const n=Buffer.from(name),data=Buffer.from(text),crc=crc32(data);
    const local=Buffer.alloc(30);local.writeUInt32LE(0x04034b50,0);local.writeUInt16LE(20,4);local.writeUInt16LE(0x800,6);local.writeUInt32LE(crc,14);local.writeUInt32LE(data.length,18);local.writeUInt32LE(data.length,22);local.writeUInt16LE(n.length,26);
    const dir=Buffer.alloc(46);dir.writeUInt32LE(0x02014b50,0);dir.writeUInt16LE(20,4);dir.writeUInt16LE(20,6);dir.writeUInt16LE(0x800,8);dir.writeUInt32LE(crc,16);dir.writeUInt32LE(data.length,20);dir.writeUInt32LE(data.length,24);dir.writeUInt16LE(n.length,28);dir.writeUInt32LE(offset,42);
    parts.push(local,n,data);central.push(dir,n);offset+=30+n.length+data.length;
  }
  const size=central.reduce((sum,b)=>sum+b.length,0),end=Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50,0);end.writeUInt16LE(entries.length,8);end.writeUInt16LE(entries.length,10);end.writeUInt32LE(size,12);end.writeUInt32LE(offset,16);
  return Buffer.concat([...parts,...central,end]);
}
const stem='generated-archive-'+Date.now().toString(36),name=stem+'.zip';
const root=(await(await request('/api/v1/files/roots')).json()).roots.find((item)=>item.available).id;
const uploaded=await request('/api/v1/files/upload?root='+root+'&path='+encodeURIComponent(name),{method:'PUT',headers:{'Content-Type':'application/octet-stream','X-Nasctl-Confirm':'1'},body:zip([['相册/第一张.txt','one'],['说明.md','# readme']])});
check(uploaded.ok,'fixture archive uploaded');
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#files',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document;
async function wait(test,label){const until=Date.now()+10000;while(Date.now()<until){if(test())return;await delay(25);}throw new Error('timed out: '+label+' '+(d.querySelector('[role=alert]')?.textContent||''));}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find((node)=>node.getAttribute('aria-label')===label||node.textContent.trim()===label);
const option=(label)=>[...d.querySelectorAll('.finder-entry')].find((node)=>node.querySelector('.entry-label').textContent===label);
w.fetch=request;w.AbortController=AbortController;w.Blob=Blob;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});w.confirm=()=>true;
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
const cleanup=async(path)=>request('/api/v1/files/trash',{method:'POST',headers:{'Content-Type':'application/json','X-Nasctl-Confirm':'1'},body:JSON.stringify({root,path})});
try{
  w.eval(await(await request('/app.js')).text());await wait(()=>option(name),'archive listed');
  option(name).click();await wait(()=>button('解压'),'details pane offers extraction for a ZIP');
  option(name).dispatchEvent(new w.MouseEvent('contextmenu',{bubbles:true,cancelable:true,clientX:20,clientY:20}));
  await wait(()=>button('解压到文件夹…'),'context menu offers extraction');button('解压到文件夹…').click();
  await wait(()=>d.querySelector('dialog [aria-label="文件操作目标"]'),'extract dialog');
  check(d.querySelector('dialog [aria-label="文件操作目标"]').value===stem,'the new folder defaults to the archive name');
  button('确认解压').click();
  await wait(()=>[...d.querySelectorAll('.task-monitor strong')].some((node)=>node.textContent==='解压'),'a background task named 解压 is shown');
  await wait(()=>option(stem),'the extracted folder appears when the task settles');
  check(await(await request('/api/v1/files/download?root='+root+'&path='+encodeURIComponent(stem+'/相册/第一张.txt'))).text()==='one','nested entry extracted with its UTF-8 name');
  // Extracting again is refused before any work starts, without touching the folder.
  option(name).dispatchEvent(new w.MouseEvent('contextmenu',{bubbles:true,cancelable:true,clientX:20,clientY:20}));
  await wait(()=>button('解压到文件夹…'),'context menu again');button('解压到文件夹…').click();
  await wait(()=>button('确认解压'),'second extract dialog');button('确认解压').click();
  await wait(()=>d.querySelector('dialog [role=alert]')?.textContent.includes('已存在'),'an existing folder is never extracted over');
  button('关闭',d.querySelector('dialog')).click();
  check(errors.length===0,'no DOM errors: '+errors.join('; '));
  console.log('ZIP extraction from the file page passed.');
}catch(failure){console.error('FAIL:',failure.message);process.exitCode=1;await delay(100);}
finally{await cleanup(stem);await cleanup(name);dom.window.close();}
