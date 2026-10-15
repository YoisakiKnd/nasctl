<script>
  import { setContext, tick } from 'svelte';
  import Icon from './Icon.svelte';
  import { fitWindow } from '../lib/windows.js';
  let { frame, app, active, mobile, areaWidth, areaHeight, onfocus, onchange, onclose, children } = $props();
  let gesture = null, notice = $state('');
  const dialogs = [];
  setContext('nasctl-window-visible', () => !frame.minimized && (!mobile || active));
  setContext('nasctl-window-dialogs', {
    register(close) { dialogs.push(close); return () => {const index=dialogs.indexOf(close); if(index>=0) dialogs.splice(index,1); notice='';}; },
    visible: () => !frame.minimized && active,
  });
  async function requestClose() {
    notice = '';
    // Let the application's existing dirty-form / pending-upload guard decide.
    if (dialogs.length) { dialogs.at(-1)(); await tick(); if (dialogs.length) {notice = '请先结束或关闭当前操作'; return;} }
    onclose();
  }
  function begin(event, kind) {
    if (mobile || frame.maximized || event.button !== 0) return;
    onfocus();
    gesture = {kind, pointer:event.pointerId, x:event.clientX,y:event.clientY,frame:{...frame}};
    event.currentTarget.setPointerCapture?.(event.pointerId);
    event.preventDefault();
  }
  function move(event) {
    if (!gesture || event.pointerId !== gesture.pointer) return;
    const dx=event.clientX-gesture.x,dy=event.clientY-gesture.y,origin=gesture.frame;
    const next=gesture.kind==='move' ? {...origin,x:origin.x+dx,y:origin.y+dy} : {...origin,width:Math.min(origin.width+dx,areaWidth-origin.x),height:Math.min(origin.height+dy,areaHeight-origin.y)};
    onchange(fitWindow(next,areaWidth,areaHeight));
  }
  function key(event, kind) {
    const steps={ArrowLeft:[-20,0],ArrowRight:[20,0],ArrowUp:[0,-20],ArrowDown:[0,20]};
    if (!steps[event.key] || mobile || frame.maximized) return;
    event.preventDefault(); const [x,y]=steps[event.key];
    onchange(fitWindow(kind==='move' ? {...frame,x:frame.x+x,y:frame.y+y} : {...frame,width:Math.min(frame.width+x,areaWidth-frame.x),height:Math.min(frame.height+y,areaHeight-frame.y)},areaWidth,areaHeight));
  }
</script>
<svelte:window onpointermove={move} onpointerup={() => gesture=null} onpointercancel={() => gesture=null}/>
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<section class="app-window" class:file-app={app.id==='files'} class:active class:maximized={frame.maximized} class:mobile class:window-hidden={frame.minimized || (mobile && !active)} style:left={frame.x+'px'} style:top={frame.y+'px'} style:width={frame.width+'px'} style:height={frame.height+'px'} style:z-index={frame.z} data-app={app.id} aria-label={app.label+'窗口'} tabindex="-1" onpointerdown={onfocus} onfocusin={onfocus}>
  <header class="window-bar">
    <button class="window-title" aria-label={app.label+'窗口标题，方向键移动，双击最大化'} onpointerdown={(event)=>begin(event,'move')} onkeydown={(event)=>key(event,'move')} ondblclick={()=>onchange({...frame,maximized:!frame.maximized})}><Icon name={app.icon} size={17}/><span>{app.label}</span></button>
    {#if notice}<span class="window-notice" role="status">{notice}</span>{/if}
    <div class="window-controls" role="toolbar" aria-label={app.label+'窗口操作'}>
      <button class="icon-button" aria-label={'最小化'+app.label} onclick={()=>onchange({...frame,minimized:true})}><Icon name="minimize" size={17}/></button>
      <button class="icon-button window-maximize" aria-label={(frame.maximized?'还原':'最大化')+app.label} onclick={()=>onchange({...frame,maximized:!frame.maximized})}><Icon name={frame.maximized?'restore':'maximize'} size={16}/></button>
      <button class="icon-button window-close" aria-label={'关闭'+app.label+'窗口'} onclick={requestClose}><Icon name="close" size={17}/></button>
    </div>
  </header>
  <div class="window-body"><div class="content" aria-label={app.label}>{@render children()}</div></div>
  {#if !frame.maximized && !mobile}<button class="window-resize" aria-label={'调整'+app.label+'窗口大小，使用方向键'} onpointerdown={(event)=>begin(event,'resize')} onkeydown={(event)=>key(event,'resize')}><Icon name="resize" size={16}/></button>{/if}
</section>
