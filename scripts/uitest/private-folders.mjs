// Real Svelte -> real console -> private album directory kept in place.
// Runs against the generated native album fixture after photo-vault.mjs.
import {readFileSync} from 'node:fs';
import {join,resolve} from 'node:path';
import {CookieJar,JSDOM,VirtualConsole} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18099',fixture=resolve(process.argv[3]||'');
if(readFileSync(join(fixture,'.nasctl-album-fixture'),'utf8')!=='nasctl-album-fixture-v1\n')throw Error('generated album fixture required');
const jar=new CookieJar(),errors=[],vc=new VirtualConsole();vc.on('jsdomError',e=>errors.push(e.message));
const request=async(path,init={})=>{const url=new URL(path,base).href,cookie=await jar.getCookieString(url);const response=await fetch(url,{...init,headers:{...init.headers,...(cookie?{Cookie:cookie}:{})}});for(const value of response.headers.getSetCookie())await jar.setCookie(value,url);return response};
const check=(ok,label)=>{if(!ok)throw Error(label);console.log('ok:',label)},delay=ms=>new Promise(r=>setTimeout(r,ms));
async function wait(fn,label){const end=Date.now()+12000;while(Date.now()<end){if(fn())return;await delay(25)}throw Error('timeout: '+label)}
const password=['generated changed vault password','generated vault password'];
let known=password[0];
// Use the vault left by photo-vault.mjs, or create one when run alone.
const status=await(await request('/api/v1/album/vault/status')).json();
if(!status.configured){known=password[1];await request('/api/v1/album/vault/create',{method:'POST',headers:{'Content-Type':'application/json','X-Nasctl-Confirm':'1'},body:JSON.stringify({password:known})});}
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#admin/album',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc,cookieJar:jar});const w=dom.window,d=w.document;
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','')};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open')};
w.localStorage.setItem('nasctl-file-pins-v1',JSON.stringify([{root:'extra',name:'Private',path:'/Private'}]));
const page=id=>d.querySelector(`.page[data-page="${id}"]`);
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find(n=>(n.getAttribute('aria-label')||n.textContent.trim())===label);
async function click(label,scope=d){await wait(()=>button(label,scope)&&!button(label,scope).disabled,label);button(label,scope).click();await delay(0)}
function input(label,value){const n=d.querySelector(`[aria-label="${label}"]`);check(n,'input '+label);n.value=value;n.dispatchEvent(new w.Event(n.tagName==='SELECT'?'change':'input',{bubbles:true}))}
const toggle=text=>[...d.querySelectorAll('dialog label')].find(n=>n.textContent.includes(text)).querySelector('input').click();
const pill='私密内容已解锁，点击立即锁定';
const names=()=>[...(page('files')?.querySelectorAll('.finder-entry .entry-label')||[])].map(n=>n.textContent);
try{
 w.eval(await(await request('/app.js')).text());await wait(()=>page('admin/album')&&button('添加目录',page('admin/album')),'album management');
 await click('添加目录');await wait(()=>d.querySelector('form.album-directory-form'),'directory editor');
 input('相册目录名称','私密照片');input('相册集合代码','secret');input('相册存储位置','extra');input('相册相对目录','Private');toggle('允许照片与系统');toggle('设为私密目录');await delay(0);
 await wait(()=>d.querySelector('[aria-label="输入保险箱密码解锁"]'),'private directory asks for the vault password');check(button('确认添加目录').disabled,'saving waits for the vault');
 input('输入保险箱密码解锁',known);await click('解锁');await wait(()=>button(pill)&&!d.querySelector('[aria-label="输入保险箱密码解锁"]'),'vault unlocked from the editor');
 d.querySelector('form.album-directory-form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
 await wait(()=>!d.querySelector('form.album-directory-form')&&[...d.querySelectorAll('.album-directory-row')].some(n=>n.textContent.includes('私密照片')&&n.querySelector('.badge.private')),'private directory saved');
 const ordinary=await(await request('/api/v1/album/media?limit=100&include_missing=true')).json();check(!ordinary.items.some(i=>i.name.startsWith('Private-')),'private photos stay out of the ordinary album');
 const scoped=await(await request('/api/v1/album/private/media')).json();check(scoped.items.length===3,'unlocked session lists the private photos');
 check((await fetch(new URL('/api/v1/album/private/media',base))).status===403,'a request without the vault cookie is refused');
 check((await fetch(new URL('/api/v1/files/list?root=extra&path=Private',base))).status===403,'the file browser refuses the folder without the vault cookie');

 w.location.hash='album';await wait(()=>[...page('album')?.querySelectorAll('.photo-tabs button')||[]].some(n=>n.textContent.includes('保险箱')),'album tabs');
 [...page('album').querySelectorAll('.photo-tabs button')].find(n=>n.textContent.includes('保险箱')).click();
 await wait(()=>page('album').querySelectorAll('.private-gallery .album-tile').length===3,'vault shows the private directory');
 page('album').querySelector('.private-gallery .album-tile').click();await wait(()=>d.querySelector('dialog .album-detail-preview img'),'private viewer');
 check(d.querySelector('dialog .album-detail-preview img').getAttribute('src').startsWith('/api/v1/album/private/media/'),'viewer reads through the private route');await click('关闭对话框');

 w.location.hash='files';await wait(()=>page('files')&&[...page('files').querySelectorAll('.finder-location')].some(n=>n.textContent.includes('Extra Space')),'file locations');
 [...page('files').querySelectorAll('.finder-location')].find(n=>n.textContent.includes('Extra Space')).click();
 await wait(()=>names().includes('Private')&&page('files').querySelector('.private-badge'),'unlocked files show the marked private folder');
 await click(pill);await wait(()=>!button(pill)&&!names().includes('Private')&&names().includes('Public-note.txt'),'locking hides the private folder from files');
 [...page('files').querySelectorAll('.finder-location')].find(n=>n.textContent.includes('Private')).click();
 await wait(()=>page('files').querySelector('.vault-unlock [aria-label="输入保险箱密码解锁"]'),'a pinned private folder asks for the password');
 input('输入保险箱密码解锁',known);page('files').querySelector('.vault-unlock-form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
 await wait(()=>names().includes('Private-0.jpg')&&button(pill),'unlocking in files opens the folder');

 await click(pill);await wait(()=>!button(pill),'locked again');
 w.location.hash='admin/album';await wait(()=>[...d.querySelectorAll('.album-directory-row')].some(n=>n.textContent.includes('私密照片')),'back to album management');
 const row=[...d.querySelectorAll('.album-directory-row')].find(n=>n.textContent.includes('私密照片'));button('移除',row).click();await delay(0);
 await wait(()=>button('确认移除目录配置')&&button('确认移除目录配置').disabled&&page('admin/album').querySelector('.directory-confirm [aria-label="输入保险箱密码解锁"]'),'removing a private directory needs the vault');
 check((await request('/api/v1/album/directories/secret',{method:'DELETE',headers:{'X-Nasctl-Confirm':'1'}})).status===403,'the server refuses removal while locked');
 input('输入保险箱密码解锁',known);page('admin/album').querySelector('.directory-confirm .vault-unlock-form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
 await click('确认移除目录配置');await wait(()=>![...d.querySelectorAll('.album-directory-row')].some(n=>n.textContent.includes('私密照片')),'private directory removed');
 check((await request('/api/v1/files/list?root=extra&path=Private')).status===200,'after removal the folder is ordinary again');
 check(errors.length===0,'no DOM errors: '+errors.join('; '));console.log('Private directories end-to-end passed.');
}finally{dom.window.close()}
