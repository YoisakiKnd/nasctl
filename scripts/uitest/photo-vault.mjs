// Real Svelte -> production local reader -> encrypted vault, generated data only.
import {readFileSync,existsSync,readdirSync} from 'node:fs';
import {join,resolve} from 'node:path';
import {createHash} from 'node:crypto';
import {CookieJar,JSDOM,VirtualConsole} from 'jsdom';
const base=process.argv[2]||'http://127.0.0.1:18123',fixture=resolve(process.argv[3]||'');
if(readFileSync(join(fixture,'.nasctl-album-fixture'),'utf8')!=='nasctl-album-fixture-v1\n')throw Error('generated album fixture required');
const jar=new CookieJar(),errors=[],vc=new VirtualConsole();vc.on('jsdomError',e=>errors.push(e.message));
const request=async(path,init={})=>{const url=new URL(path,base).href,cookie=await jar.getCookieString(url);const response=await fetch(url,{...init,headers:{...init.headers,...(cookie?{Cookie:cookie}:{})}});for(const value of response.headers.getSetCookie())await jar.setCookie(value,url);return response};
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/#album',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc,cookieJar:jar});const w=dom.window,d=w.document;
w.fetch=request;w.AbortController=AbortController;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','')};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open')};
const check=(ok,label)=>{if(!ok)throw Error(label);console.log('ok:',label)},delay=ms=>new Promise(r=>setTimeout(r,ms));
async function wait(fn,label){const end=Date.now()+12000;while(Date.now()<end){if(fn())return;await delay(25)}throw Error('timeout: '+label)}
const button=(label,scope=d)=>[...scope.querySelectorAll('button')].find(n=>(n.getAttribute('aria-label')||n.textContent.trim())===label);
async function click(label){await wait(()=>button(label)&&!button(label).disabled,label);button(label).click();await delay(0)}
function input(label,value){const n=d.querySelector('input[aria-label="'+label+'"]');check(n,'input '+label);n.value=value;n.dispatchEvent(new w.Event('input',{bubbles:true}))}
function submit(){d.querySelector('.vault-password-form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}))}
const hash=body=>createHash('sha256').update(body).digest('hex');
const password='generated vault password',newPassword='generated changed vault password';
try{
 const originalName='Photo-054.jpg',page=await(await request('/api/v1/album/media?limit=48')).json(),item=page.items.find(i=>i.name===originalName);check(item,'generated photo is indexed');const original=Buffer.from(await(await request(item.raw_url)).arrayBuffer());
 w.eval(await(await request('/app.js')).text());await wait(()=>d.querySelectorAll('.album-tile').length===48,'gallery');await click('查看 '+originalName);await wait(()=>button('移入保险箱'),'photo viewer');await click('移入保险箱');await wait(()=>d.querySelector('.vault-password-form'),'private password creation');
 input('保险箱密码',password);input('再次输入保险箱密码',password);check(d.querySelector('.vault-password-form').checkValidity(),'vault setup native validity');submit();await wait(()=>button('解锁保险箱')&&!button('解锁保险箱').disabled,'created vault is locked');
 input('保险箱密码',password);submit();await wait(()=>button('确认加密并移入'),'unlocked candidate');check(!d.cookie.includes('nasctl_photo_vault'),'vault credential is HttpOnly');await click('确认加密并移入');await wait(()=>d.querySelectorAll('.vault-gallery .album-tile').length===1&&!button('确认加密并移入'),'encrypted move completes');
 check(!existsSync(join(fixture,'library',originalName)),'source plaintext removed after verified encryption');
 const privatePath=join(fixture,'album-state/.nasctl-photo-vault');for(const name of readdirSync(privatePath)){const body=readFileSync(join(privatePath,name));check(!body.includes(original)&&!body.includes(Buffer.from(originalName))&&!body.includes(Buffer.from(password)),'ciphertext contains no plaintext: '+name)}
 for(const part of ['', '/thumb','/raw','/download'])check((await request('/api/v1/album/media/'+item.id+part)).status===404,'old public link denied '+(part||'details'));
 const publicPage=await(await request('/api/v1/album/media?include_missing=true&limit=100')).json();check(!publicPage.items.some(i=>i.id===item.id),'include_missing cannot reveal private item');
 const privateList=await(await request('/api/v1/album/vault/media')).json();check(privateList.items.length===1&&!privateList.items[0].pending,'private listing has completed item');const privateItem=privateList.items[0];
 const raw='/api/v1/album/vault/media/'+privateItem.id+'/raw';const response=await request(raw,{headers:{Range:'bytes=1-15'}});check(response.status===206&&Buffer.from(await response.arrayBuffer()).equals(original.subarray(1,16)),'encrypted original supports authenticated byte ranges');
 check((await fetch(base+raw)).status===403,'copied link without vault cookie denied');
 const recovered=Buffer.from(await(await request('/api/v1/album/vault/media/'+privateItem.id+'/download')).arrayBuffer());check(hash(recovered)===hash(original),'download decrypts exactly the original bytes');
 await click('查看私密照片 '+originalName);await wait(()=>d.querySelector('dialog .album-detail-preview img'),'private lightbox');check(d.querySelector('dialog img').getAttribute('src')===raw,'private lightbox uses gated endpoint');await click('关闭对话框');await click('锁定');await wait(()=>button('解锁保险箱'),'manual lock');check(!d.querySelector('.vault-gallery img')&&!d.querySelector('dialog'),'locking clears photos and viewer from DOM');check((await request(raw)).status===403,'revoked cookie cannot read original');
 input('保险箱密码','wrong-password');submit();await wait(()=>d.querySelector('.photo-vault [role=alert]'),'wrong password message');check(d.querySelectorAll('.primary-nav .nav-link').length===4,'wrong vault password does not sign out NAS');input('保险箱密码',password);submit();await wait(()=>button('修改密码'),'re-unlock');await click('修改密码');input('保险箱密码',password);input('保险箱新密码',newPassword);input('再次输入保险箱密码',newPassword);submit();await wait(()=>button('解锁保险箱')&&!button('解锁保险箱').disabled,'password rotated and locked');
 input('保险箱密码',newPassword);submit();await wait(()=>d.querySelector('.vault-gallery img'),'new password restores library');await click('全部照片');await wait(()=>d.querySelectorAll('.album-tile').length===48,'return to ordinary library');await delay(100);check(!(await(await request('/api/v1/album/vault/status')).json()).unlocked,'leaving locks vault');
 check(![...d.querySelectorAll('.album-tile')].some(n=>n.getAttribute('aria-label')==='查看 '+originalName),'private photo absent from ordinary gallery');check((await request(raw)).status===403,'leaving revokes private original access');
 check(errors.length===0,'no DOM errors: '+errors.join('; '));console.log('Encrypted photo vault end-to-end passed.');
}finally{dom.window.close()}
