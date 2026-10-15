// Integration: mounted app instances keep drafts while the desktop switches focus.
// Runs only on a development machine against an isolated local NAS fixture.
import {JSDOM,VirtualConsole} from 'jsdom';
import assert from 'node:assert/strict';
import {fitWindow,newWindow} from '../../frontend/src/lib/windows.js';
const base=process.argv[2]||'http://127.0.0.1:18094';
const token=process.argv[3]||'',headers=token?{Authorization:`Bearer ${token}`}:{},request=(path,options={})=>fetch(new URL(path,base),{...options,headers:{...options.headers,...headers}});
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const dom=new JSDOM(await(await request('/')).text(),{url:base+'/',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{assert.ok(ok,label);console.log('ok:',label);};
async function wait(predicate,label){const until=Date.now()+8000;while(Date.now()<until){if(predicate())return;await delay(20);}throw new Error('timed out: '+label);}
const app=(id)=>d.querySelector(`.app-window[data-app="${id}"]`);
const launch=(id)=>d.querySelectorAll('.desktop-app')[['overview','files','album','media','backup','jobs','settings'].indexOf(id)].click();
const named=(label,scope=d)=>scope.querySelector(`button[aria-label="${label}"]`);
const button=(label,scope=d)=>[...(scope?.querySelectorAll('button')||[])].find((node)=>node.textContent.trim()===label);
async function click(node){check(node,'control exists');node.click();await delay(0);}
w.fetch=request;w.AbortController=AbortController;
w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
function keyboard(node,key){node.dispatchEvent(new w.KeyboardEvent('keydown',{key,bubbles:true,cancelable:true}));}
try{
  // Geometry properties matter on short phones and resized desktop browser panes.
  for(const [width,height] of [[390,300],[1280,576],[320,200]]){
    for(const frame of [newWindow('files',5,width,height,1),fitWindow({x:-90,y:4000,width:5000,height:9000},width,height)]){
      check(frame.x>=0&&frame.y>=0&&frame.x+frame.width<=width&&frame.y+frame.height<=height,'window remains within '+width+'x'+height);
    }
  }
  w.eval(await(await request('/app.js')).text());await wait(()=>d.querySelectorAll('.desktop-app').length===7,'desktop launchers');
  check(!d.querySelector('.app-window'),'home opens as a desktop without an unsolicited app window');
  launch('media');await wait(()=>button('添加订阅',app('media'))&&!button('添加订阅',app('media')).disabled,'anime ready');
  await click(button('添加订阅',app('media')));await wait(()=>app('media').querySelector('dialog[open]'),'editor');
  const title=app('media').querySelector('input[name="title"]');title.value='Unsaved multiwindow draft';title.dispatchEvent(new w.Event('input',{bubbles:true}));await delay(0);
  launch('files');await wait(()=>app('files')?.classList.contains('active'),'file app focus');
  launch('jobs');await wait(()=>app('jobs')?.classList.contains('active'),'jobs focus');
  check(d.querySelectorAll('.app-window').length===3,'three apps coexist');
  check(Number(app('jobs').style.zIndex)>Number(app('files').style.zIndex),'focused window is above other windows');
  check(title.isConnected&&title.value==='Unsaved multiwindow draft','switching apps preserves the mounted draft');
  await click(named('切换到追番'));
  await click(named('最小化追番',app('media')));check(app('media').classList.contains('window-hidden'),'minimize hides without unmount');
  await click(named('切换到追番，已最小化'));check(!app('media').classList.contains('window-hidden')&&title.value==='Unsaved multiwindow draft','dock restores the same editor');
  await click(named('关闭追番窗口',app('media')));check(app('media')&&app('media').textContent.includes('放弃'),'closing a dirty app requires its existing discard confirmation');
  // The editor belongs to its application, leaving the dock and other windows usable.
  check(app('media').querySelector('.window-modal-layer')&&!d.body.classList.contains('modal-open'),'editor blocks only its own application');
  await click(named('切换到任务中心'));check(app('jobs').classList.contains('active'),'other apps remain usable while an editor is open');
  await click(named('切换到追番'));
  const keep=[...app('media').querySelectorAll('button')].find((node)=>node.textContent.includes('继续编辑'));await click(keep);
  check(title.value==='Unsaved multiwindow draft','canceling close preserves changes');
  await click(named('最大化追番',app('media')));check(app('media').classList.contains('maximized'),'maximize');
  await click(named('还原追番',app('media')));check(!app('media').classList.contains('maximized'),'restore');
  const beforeWidth=parseFloat(app('media').style.width),handle=app('media').querySelector('.window-resize');keyboard(handle,'ArrowLeft');await delay(0);check(parseFloat(app('media').style.width)===beforeWidth-20,'keyboard resize works with editor open');
  const titlebar=app('media').querySelector('.window-title');keyboard(titlebar,'ArrowDown');await delay(0);check(parseFloat(app('media').style.top)>0,'keyboard movement works');
  const pointer=(type,x,y)=>new w.MouseEvent(type,{clientX:x,clientY:y,button:0,bubbles:true,cancelable:true});
  const x=parseFloat(app('media').style.left);titlebar.dispatchEvent(pointer('pointerdown',100,100));w.dispatchEvent(pointer('pointermove',120,100));w.dispatchEvent(pointer('pointerup',120,100));await delay(0);check(parseFloat(app('media').style.left)===x+20,'pointer drag updates bounded position');
  Object.defineProperty(w,'innerWidth',{configurable:true,value:390});Object.defineProperty(w,'innerHeight',{configurable:true,value:650});w.dispatchEvent(new w.Event('resize'));await delay(0);
  check(d.querySelector('.desktop-home').inert === true,'covered phone desktop is removed from keyboard navigation');
  check(app('media').classList.contains('mobile')&&!app('media').classList.contains('window-hidden')&&app('files').classList.contains('window-hidden'),'phone shows focused app as a full-screen window');
  await click(named('切换到文件管理'));check(!app('files').classList.contains('window-hidden')&&app('media').classList.contains('window-hidden'),'phone dock switches apps and retains hidden editor');
  await click(d.querySelector('.desktop-dock button[aria-label="显示桌面"]'));check([...d.querySelectorAll('.app-window')].every((node)=>node.classList.contains('window-hidden')),'show desktop minimizes all apps');
  await click(named('切换到追番，已最小化'));check(title.value==='Unsaved multiwindow draft','returning from desktop preserves draft');
  w.dispatchEvent(new w.CustomEvent('nasctl:unauthorized'));await delay(0);check(!d.querySelector('.app-window')&&!title.isConnected,'expired session unmounts all protected app state');
  check(errors.length===0,'no DOM errors: '+errors.join('; '));
  console.log('Multiwindow desktop flows passed; no subscription or file was saved by this test.');
}finally{dom.window.close();}
