// Real browser-session cookies against an isolated token-enabled Go service.
import { CookieJar, JSDOM, VirtualConsole } from 'jsdom';
const base = process.argv[2] || 'http://127.0.0.1:18091';
const token = process.argv[3];
if (!token) throw new Error('usage: node auth.mjs BASE FIXTURE_TOKEN');
const jar = new CookieJar(), errors = [];
const vc = new VirtualConsole();
vc.on('jsdomError', (error) => errors.push(error.message));
const html = await fetch(base + '/');
if (!html.ok) throw new Error('public shell must load without a credential');
const dom = new JSDOM(await html.text(), {url:base+'/#admin/anime', runScripts:'outside-only', pretendToBeVisual:true, virtualConsole:vc, cookieJar:jar});
const w = dom.window, d = w.document;
const delay = (ms) => new Promise((resolve) => setTimeout(resolve,ms));
const check = (ok,label) => {if (!ok) throw new Error(label); console.log('ok:',label);};
async function wait(predicate,label) {const until=Date.now()+6000; while(Date.now()<until) {if(predicate()) return; await delay(25);} throw new Error('timed out: '+label);}
const text = (selector) => d.querySelector(selector)?.textContent || '';
const byText = (label,scope=d.querySelector('.page.active')||d) => [...scope.querySelectorAll('button')].find((button)=>button.textContent.trim()===label);
function input(selector,value) {const element=d.querySelector(selector); check(element,'input '+selector); element.value=value; element.dispatchEvent(new w.Event('input',{bubbles:true}));}
function submit() {d.querySelector('.login-card form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));}
w.fetch = async (path,init={}) => {
  const url=new URL(path,base).href;
  const cookies=await jar.getCookieString(url);
  const response=await fetch(url,{...init,headers:{...init.headers,...(cookies?{Cookie:cookies}:{})}});
  for (const value of response.headers.getSetCookie()) await jar.setCookie(value,url);
  return response;
};
w.AbortController=AbortController;
w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};
w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
w.confirm=()=>{throw new Error('native confirmation must not be used');};
try {
  w.eval(await (await fetch(base+'/app.js')).text());
  await wait(()=>d.querySelector('#access-token'),'login form');
  check(!d.querySelector('.nav-link'),'protected console is not mounted before login');
  input('#access-token','wrong-token'); submit();
  await wait(()=>text('.login-card [role="alert"]').includes('不正确'),'invalid credential feedback');
  input('#access-token',token); submit();
  await wait(()=>d.querySelector('.nav-link') && byText('添加订阅')?.disabled===false,'authenticated console');
  check(!w.document.cookie.includes('nasctl_session'),'session cookie is HttpOnly');
  check(!Object.values(w.localStorage).includes(token),'access token is not stored in localStorage');
  const savedCookie=await jar.getCookieString(base);
  check(savedCookie.includes('nasctl_session=')&&!savedCookie.includes(token),'session cookie differs from API token');
  byText('添加订阅').click();
  await wait(()=>d.querySelector('dialog[open]'),'subscription draft');
  input('input[name="title"]','Disposable unsaved draft');
  byText('取消',d.querySelector('dialog')).click();
  await wait(()=>byText('继续编辑'),'inline discard confirmation');
  byText('继续编辑').click();
  await wait(()=>!byText('放弃修改'),'resume draft');
  check(d.querySelector('input[name="title"]').value==='Disposable unsaved draft','draft survives canceled dismissal');
  byText('取消',d.querySelector('dialog')).click();
  await wait(()=>byText('放弃修改'),'second discard confirmation');
  byText('放弃修改').click();
  await wait(()=>!d.querySelector('dialog'),'draft discarded without native confirmation');
  w.location.hash='me';
  await wait(()=>byText('退出登录'),'account page');
  byText('退出登录').click();
  await wait(()=>d.querySelector('#access-token'),'logout returns to login');
  const revoked=await fetch(base+'/api/v1/anime/status',{headers:{Cookie:savedCookie}});
  check(revoked.status===401,'copied session credential is revoked on logout');
  input('#access-token',token); submit();
  await wait(()=>d.querySelector('.nav-link'),'second login');
  check(d.querySelector('.page.active')?.dataset.page==='me','the requested page survives re-login');
  await w.fetch('/api/v1/auth/logout',{method:'POST'});
  w.location.hash='files';
  await wait(()=>d.querySelector('#access-token'),'401 unmounts protected console and requests login');
  check(!d.querySelector('.nav-link'),'expired identity cannot keep protected views mounted');
  check(errors.length===0,'no DOM runtime errors: '+errors.join('; '));
  console.log('Browser session and draft preservation flows passed.');
} finally {dom.window.close();}
