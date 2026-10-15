// Post-restart proof for accounts.mjs's generated fixture. No owner credentials
// are accepted from arguments or persistent frontend storage.
import {readFileSync} from 'node:fs';
import {join,resolve} from 'node:path';
import {CookieJar,JSDOM,VirtualConsole} from 'jsdom';
const fixture=resolve(process.argv[2]||''),meta=JSON.parse(readFileSync(join(fixture,'fixture.json'),'utf8'));
if(meta.marker!=='nasctl-accounts-fixture-v1'||!/^http:\/\/127\.0\.0\.1:\d+$/.test(meta.base))throw Error('generated fixture required');
const base=meta.base,check=(ok,label)=>{if(!ok)throw Error(label);console.log('ok:',label)},pause=ms=>new Promise(r=>setTimeout(r,ms));
async function wait(fn,label){const end=Date.now()+12000;while(Date.now()<end){if(fn())return;await pause(25)}throw Error('timeout: '+label)}
const status=await(await fetch(base+'/api/v1/auth/status')).json();check(status.accounts_enabled&&!status.setup_required&&!status.authenticated,'account requirement survives process restart');
check((await fetch(base+'/api/v1/files/roots',{headers:{Cookie:readFileSync(join(fixture,'before-restart.cookie'),'utf8')}})).status===401,'live owner session from before restart is revoked');
const jar=new CookieJar(),errors=[],vc=new VirtualConsole();vc.on('jsdomError',e=>errors.push(e.message));
const dom=new JSDOM(await(await fetch(base+'/')).text(),{url:base+'/#files',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc,cookieJar:jar}),w=dom.window,d=w.document;
w.fetch=async(path,init={})=>{const url=new URL(path,base).href,cookie=await jar.getCookieString(url),res=await fetch(url,{...init,headers:{...init.headers,...(cookie?{Cookie:cookie}:{})}});for(const value of res.headers.getSetCookie())await jar.setCookie(value,url);return res};
w.AbortController=AbortController;w.TextEncoder=TextEncoder;w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','')};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open')};
const input=(selector,value)=>{const n=d.querySelector(selector);if(!n)throw Error('missing login input');n.value=value;n.dispatchEvent(new w.Event('input',{bubbles:true}))};
try{
 w.eval(await(await fetch(base+'/app.js')).text());await wait(()=>d.querySelector('#account-username'),'member login after restart');input('#account-username','reader');input('#account-password','generated reader changed password');d.querySelector('.login-card form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));await wait(()=>d.querySelectorAll('.nav-link').length===4&&d.querySelector('.finder-layout'),'reader files after restart');
 const roots=await(await w.fetch('/api/v1/files/roots')).json();check(roots.roots.length===1&&roots.roots[0].id==='data'&&!roots.roots[0].writable&&!roots.writable,'saved storage active with reader write capability removed');
 await wait(()=>d.body.textContent.includes('generated.txt'),'persisted storage directory is browsable');const marker=await(await w.fetch('/api/v1/files/download?root=data&path=.nasctl-ui-fixture')).text();check(marker==='nasctl-accounts-fixture-v1\n','server points to the exact owned fixture');
 const file=await(await w.fetch('/api/v1/files/download?root=data&path=generated.txt')).text();check(file==='original account fixture bytes\n','original data bytes remain correct');
 const viewerCookie=await jar.getCookieString(base);await w.fetch('/api/v1/auth/logout',{method:'POST'});check((await fetch(base+'/api/v1/files/roots',{headers:{Cookie:viewerCookie}})).status===401,'post-restart session revoked on logout');
 check(!errors.length,'no post-restart DOM runtime errors: '+errors.join('; '));console.log('Persistent members, password change, storage activation and reader browser flow passed.');
}finally{dom.window.close()}
