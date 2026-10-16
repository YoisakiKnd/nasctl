// Integration: user pages and admin pages share one shell. Visited pages stay
// mounted while hidden, so drafts survive navigation between them and between
// the user and admin areas. Runs only on a development machine against an
// isolated local NAS fixture.
import {JSDOM,VirtualConsole} from 'jsdom';
import assert from 'node:assert/strict';
import {resolve,allowedAdminPages} from '../../frontend/src/lib/routes.js';
const base=process.argv[2]||'http://127.0.0.1:18094';
const token=process.argv[3]||'',headers=token?{Authorization:`Bearer ${token}`}:{},request=(path,options={})=>fetch(new URL(path,base),{...options,headers:{...options.headers,...headers}});
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{assert.ok(ok,label);console.log('ok:',label);};
async function wait(predicate,label){const until=Date.now()+8000;while(Date.now()<until){if(predicate())return;await delay(20);}throw new Error('timed out: '+label);}
const page=(id)=>d.querySelector(`.page[data-page="${id}"]`);
const active=()=>d.querySelector('.page.active')?.dataset.page;
const button=(label,scope=d)=>[...(scope?.querySelectorAll('button')||[])].find((node)=>node.textContent.trim()===label);
async function go(hash,id=hash){w.location.hash=hash;await wait(()=>active()===id,'route '+hash);}
w.fetch=request;w.AbortController=AbortController;
w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
try{
  // Route resolution is pure: aliases keep old links working, roles gate admin pages.
  const viewer={authenticated:true,role:'viewer',can_manage_device:false},operator={authenticated:true,role:'operator',can_manage_device:false},admin={authenticated:true,role:'admin',can_manage_device:true};
  check(resolve('#media',viewer)==='anime'&&resolve('#overview',viewer)==='home'&&resolve('#admin/jobs',viewer)==='home','viewers stay on user pages');
  check(resolve('#jobs',operator)==='admin/jobs'&&resolve('#admin/album',operator)==='admin/album'&&resolve('#settings',operator)==='home'&&resolve('#admin/users',operator)==='home','operators reach task pages but not device settings');
  check(resolve('#settings',admin)==='admin/system'&&resolve('#backup',admin)==='admin/backup'&&resolve('#nonsense',admin)==='home','old desktop hashes map to their new pages');
  check(allowedAdminPages(viewer).length===0&&allowedAdminPages(operator).length===4&&allowedAdminPages(admin).length===8,'admin navigation follows the role');

  w.eval(await(await request('/app.js')).text());await wait(()=>active()==='home','home');
  check(d.querySelector('.app-header .primary-nav')&&!d.querySelector('.admin-sidebar'),'user pages use the top navigation');
  check(d.querySelectorAll('.page').length===1,'only the visited page is mounted');

  await go('admin/anime');await wait(()=>button('添加订阅',page('admin/anime'))&&!button('添加订阅',page('admin/anime')).disabled,'anime management ready');
  check(d.querySelector('.admin-sidebar')&&!d.querySelector('.app-header'),'admin pages use the sidebar');
  button('添加订阅',page('admin/anime')).click();await wait(()=>page('admin/anime').querySelector('dialog[open]'),'editor');
  const title=page('admin/anime').querySelector('input[name="title"]');title.value='Unsaved shell draft';title.dispatchEvent(new w.Event('input',{bubbles:true}));await delay(0);
  check(page('admin/anime').querySelector('.window-modal-layer')&&!d.body.classList.contains('modal-open'),'the editor belongs to its page instead of the whole document');

  await go('files');await wait(()=>!page('files').querySelector('.loading-state'),'files ready');
  check(page('admin/anime').hidden&&title.isConnected&&title.value==='Unsaved shell draft','leaving for a user page keeps the hidden admin draft');
  await go('admin/jobs');check(page('files').hidden&&!page('files').querySelector('.loading-state'),'files stay mounted behind admin pages');
  await go('admin/anime');check(title.value==='Unsaved shell draft'&&page('admin/anime').querySelector('dialog[open]'),'returning restores the same editor');
  button('取消',page('admin/anime').querySelector('dialog')).click();await wait(()=>button('放弃修改',page('admin/anime')),'dirty editor asks before discarding');
  button('放弃修改',page('admin/anime')).click();await wait(()=>!page('admin/anime').querySelector('dialog'),'draft discarded');

  await go('settings','admin/system');check(d.querySelector('.sidebar-nav .nav-link.active')?.textContent.includes('系统设置'),'alias highlights the resolved sidebar entry');
  check(d.querySelector('.drawer-toggle')&&!d.querySelector('.admin-sidebar.open'),'drawer starts closed');
  d.querySelector('.drawer-toggle').click();await delay(0);check(d.querySelector('.admin-sidebar.open')&&d.querySelector('.drawer-scrim'),'narrow screens open the sidebar as a drawer');
  d.querySelector('.drawer-scrim').click();await delay(0);check(!d.querySelector('.admin-sidebar.open'),'the scrim closes the drawer');
  d.querySelector('.sidebar-exit').click();await wait(()=>active()==='home','back to the user area');
  check([...d.querySelectorAll('.tab-bar a')].map((node)=>node.textContent.trim()).join()==='首页,文件,相册,追番,我的','phones get a bottom tab bar with every user page');

  await go('me');check(button('深色',page('me')),'theme choices are on the account page');
  button('深色',page('me')).click();await delay(0);check(d.documentElement.dataset.theme==='dark'&&w.localStorage.getItem('nasctl-theme')==='dark','explicit dark theme is applied and remembered');
  button('跟随系统',page('me')).click();await delay(0);check(d.documentElement.dataset.theme==='light'&&w.localStorage.getItem('nasctl-theme')===null,'following the system forgets the explicit choice');

  w.dispatchEvent(new w.CustomEvent('nasctl:unauthorized'));await delay(0);check(!d.querySelector('.page')&&!title.isConnected,'expired session unmounts every page');
  check(errors.length===0,'no DOM errors: '+errors.join('; '));
  console.log('Shell navigation, keep-alive pages and role routing passed; nothing was saved by this test.');
}finally{dom.window.close();}
