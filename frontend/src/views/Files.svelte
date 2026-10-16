<script>
  import ErrorNotice from '../components/ErrorNotice.svelte';
  import { getContext, onMount, tick, untrack } from 'svelte';
  import { api, query } from '../lib/api.js';
  import { bytes, time } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import FileGlyph from '../components/FileGlyph.svelte';
  import FilePreview from '../components/FilePreview.svelte';
  import Dialog from '../components/Dialog.svelte';
  import FileOperation from '../components/FileOperation.svelte';
  import FileBatch from '../components/FileBatch.svelte';
  import FileBatchResult from '../components/FileBatchResult.svelte';
  import TaskMonitor from '../components/TaskMonitor.svelte';
  let { readOnly=false, agent=false, searchRequest=null } = $props();
  const host=getContext('nasctl-window-dialogs');
  const paneID=$props.id();
  let roots=$state([]),root=$state(''),path=$state('/'),page=$state(1),sort=$state('name:asc'),search=$state(''),appliedSearch=$state('');
  let listing=$state(null),loading=$state(true),error=$state(''),trash=$state(false),scope=$state('folder');
  let selected=$state(null),previewInfo=$state(null),previewError=$state(''),fullPreview=$state(false);
  let selection=$state([]),multiMode=$state(false),anchorIndex=0,batchResult=$state(null);
  let limits=$state({copy_max_entries:10000,copy_max_bytes:8*1024**3,batch_max_items:100,max_page_size:200,search_max_results:200});
  const pageSize=$derived(Math.min(50,limits.default_page_size||50,limits.max_page_size));
  const selectionWritable=$derived(selection.length>0&&!readOnly&&!agent&&selection.every((item)=>roots.some((r)=>r.id===item._root&&r.writable&&r.available)));
  let dragging=$state(false),action=$state(null),message=$state(''),task=$state(''),uploadMax=$state(8*1024**3);
  let view=$state('grid'),details=$state(true),locations=$state(false),pins=$state([]),recent=$state([]),menu=$state(null);
  let tabs=$state([{id:1,location:null,back:[],forward:[]}]),activeTab=$state(1),tabNumber=1;
  let explorer=$state(),grid=$state(),menuElement=$state(),columns=$state(1),focusIndex=$state(0);
  let controller,previewRequest,previewTimer,generation=0,previewGeneration=0,disposed=false,lastSearch;
  const selectedRoot=$derived(roots.find((item)=>item.id===root));
  const selectedID=$derived(selected?._root || root);
  const canWrite=$derived(Boolean(selectedRoot?.writable && selectedRoot?.available && !readOnly && !agent));
  const selectedWritable=$derived(Boolean(roots.find((item)=>item.id===selectedID&&item.available)?.writable && !readOnly && !agent));
  const canCopy=$derived(!readOnly && !agent && roots.some((item)=>item.writable && item.available));
  const currentTab=$derived(tabs.find((item)=>item.id===activeTab));
  const crumbs=$derived(path.split('/').filter(Boolean));
  const entries=$derived(scope==='recent'?recent:scope==='pinned'?pins.map((pin)=>({name:pin.name,path:pin.path,is_dir:true,kind:'directory',_root:pin.root})):listing?.entries || []);
  const title=$derived(trash?'回收站':scope==='recent'?'最近使用':scope==='pinned'?'已固定':crumbs.at(-1)||selectedRoot?.label||'我的文件');
  const searching=$derived(Boolean(listing?.search));
  const pinned=$derived(pins.some((pin)=>pin.root===root && pin.path===path));
  const kindLabel=(entry)=>entry.is_dir?'文件夹':({image:'图像',video:'视频',audio:'音频',text:'文本文档',archive:'压缩文件'})[entry.kind]||'文件';
  const relative=(value)=>value.replace(/^\/+/, '') || '/';
  const rootLabel=(value)=>roots.find((item)=>item.id===value)?.label || value;
  const download=(entry)=>query('/api/v1/files/download',{root:entry._root || root,path:relative(entry.path)});
  // Folders download as an uncompressed ZIP streamed by the NAS.
  const archive=(entry)=>query('/api/v1/files/archive',{root:entry._root || root,path:relative(entry.path)});
  const previewURL=$derived(selected?query('/api/v1/files/preview',{root:selectedID,path:relative(selected.path)}):'');
  function capture(){return {root,path,page,sort,search,appliedSearch,trash,scope};}
  function storeTab(){tabs=tabs.map((tab)=>tab.id===activeTab?{...tab,location:capture()}:tab);}
  function clearSelection(){++previewGeneration;clearTimeout(previewTimer);previewRequest?.abort();selected=null;selection=[];previewInfo=null;previewError='';fullPreview=false;focusIndex=0;menu=null;}
  const selectionKey=(entry)=>String(entry._root||root)+'\0'+(entry.id||entry.path);
  const isSelected=(entry)=>selection.some((item)=>selectionKey(item)===selectionKey(entry));
  function chooseSelection(items){
    if(items.length>limits.batch_max_items){message=`一次最多选择 ${limits.batch_max_items} 项，请缩小选择范围。`;return false;}
    selection=items.map((item)=>({...item,_root:item._root||root}));return true;
  }
  function selectAll(){const items=trash?listing?.items||[]:entries;if(chooseSelection(items)){selected=items.length===1&&!trash?items[0]:null;++previewGeneration;clearTimeout(previewTimer);previewRequest?.abort();previewInfo=null;previewError='';}}
  async function load(focus=false){
    const current=++generation;controller?.abort();controller=new AbortController();loading=true;error='';listing=null;
    if(scope!=='folder'||!root){loading=false;return;}
    const [key,order]=sort.split(':');
    try{
      const [data,rootData]=await Promise.all([api(query(trash?'/api/v1/files/trash':appliedSearch?'/api/v1/files/search':'/api/v1/files/list',{root,path:relative(path),q:appliedSearch||undefined,page,page_size:pageSize,sort:key,order}),{signal:controller.signal}),api('/api/v1/files/roots',{signal:controller.signal}).catch(()=>null)]);
      if(!disposed&&current===generation){if(rootData)roots=rootData.roots||[];listing={...data,search:appliedSearch};loading=false;if(focus===true&&(!host||host.visible())){await tick();explorer?.querySelector('[role="option"]')?.focus();}}
    }catch(failure){if(!disposed&&current===generation&&failure.name!=='AbortError')error=failure.message;}
    finally{if(!disposed&&current===generation)loading=false;}
  }
  function applyLocation(next,focus=false){
    clearSelection();root=next.root;path=next.path;page=next.page;sort=next.sort;search=next.search;appliedSearch=next.appliedSearch;trash=next.trash;scope=next.scope;locations=false;storeTab();load(focus);
  }
  function go(next,focus=false){
    const before=capture();tabs=tabs.map((tab)=>tab.id===activeTab?{...tab,back:[...tab.back,before].slice(-50),forward:[]}:tab);
    applyLocation({...before,page:1,search:'',appliedSearch:'',trash:false,scope:'folder',...next},focus);
  }
  function navigate(next,focus=false){go({path:next},focus);}
  function historyMove(direction){
    const tab=currentTab,items=tab[direction];if(!items.length)return;
    const next=items.at(-1),other=direction==='back'?'forward':'back';
    tabs=tabs.map((item)=>item.id===activeTab?{...item,[direction]:items.slice(0,-1),[other]:[...item[other],capture()].slice(-50)}:item);applyLocation(next);
  }
  function switchTab(tab){if(tab.id===activeTab)return;storeTab();activeTab=tab.id;applyLocation(tab.location);}
  function addTab(){if(tabs.length>=8)return;storeTab();const location={...capture(),path:'/',page:1,search:'',appliedSearch:'',trash:false,scope:'folder'};const tab={id:++tabNumber,location,back:[],forward:[]};tabs=[...tabs,tab];activeTab=tab.id;applyLocation(location);}
  async function tabKeys(event,index){
    const next=({ArrowLeft:Math.max(0,index-1),ArrowRight:Math.min(tabs.length-1,index+1),Home:0,End:tabs.length-1})[event.key];
    if(next===undefined)return;event.preventDefault();switchTab(tabs[next]);await tick();explorer?.querySelectorAll('[role="tab"]')[next]?.focus();
  }
  function closeTab(tab){if(tabs.length===1)return;const next=tabs.find((item)=>item.id!==tab.id);if(tab.id===activeTab)switchTab(next);tabs=tabs.filter((item)=>item.id!==tab.id);}
  function tabLabel(tab){const loc=tab.id===activeTab?capture():tab.location;return loc?.trash?'回收站':loc?.scope==='recent'?'最近使用':loc?.scope==='pinned'?'已固定':loc?.path.split('/').filter(Boolean).at(-1)||rootLabel(loc?.root)||'我的文件';}
  function remember(entry){const item={...entry,_root:entry._root||root};recent=[item,...recent.filter((old)=>old._root!==item._root||old.path!==item.path)].slice(0,30);}
  function select(entry,index=entries.indexOf(entry),event={}){
    let items;
    if(event.shiftKey&&index>=0){items=entries.slice(Math.min(anchorIndex,index),Math.max(anchorIndex,index)+1);}
    else if(event.metaKey||event.ctrlKey||multiMode&&!event.keyboardSingle){items=isSelected(entry)?selection.filter((item)=>selectionKey(item)!==selectionKey(entry)):[...selection,entry];anchorIndex=index;}
    else {items=[entry];anchorIndex=index;}
    if(!chooseSelection(items))return;
    if(items.length!==1||trash){++previewGeneration;clearTimeout(previewTimer);previewRequest?.abort();selected=null;previewInfo=null;previewError='';focusIndex=Math.max(0,index);return;}
    entry=items[0];
    ++previewGeneration;const current=previewGeneration;clearTimeout(previewTimer);previewRequest?.abort();selected=entry;previewInfo=null;previewError='';focusIndex=Math.max(0,index);
    const rootID=entry._root||root;
    previewTimer=setTimeout(async()=>{
      previewRequest=new AbortController();
      try{const info=await api(query('/api/v1/files/stat',{root:rootID,path:relative(entry.path)}),{signal:previewRequest.signal});if(!disposed&&current===previewGeneration)previewInfo=info;}
      catch(failure){if(!disposed&&current===previewGeneration&&failure.name!=='AbortError')previewError=failure.message;}
    },150);
  }
  function open(entry,keyboard=false){
    remember(entry);
    if(entry.is_dir){go({root:entry._root||root,path:entry.path},keyboard);return;}
    if(selected!==entry)select(entry);fullPreview=true;
  }
  async function fileKeys(event,index){
    if(event.key==='Enter'){event.preventDefault();open(entries[index],true);return;}
    if(event.key===' '){event.preventDefault();select(entries[index],index,{ctrlKey:true});return;}
    if(event.key==='ContextMenu'||(event.shiftKey&&event.key==='F10')){event.preventDefault();showMenu(event,entries[index]);return;}
    const step=view==='grid'?columns:1,target=({ArrowRight:index+1,ArrowLeft:index-1,ArrowDown:index+step,ArrowUp:index-step,Home:0,End:entries.length-1})[event.key];
    if(target===undefined)return;event.preventDefault();const next=Math.min(entries.length-1,Math.max(0,target));select(entries[next],next,{shiftKey:event.shiftKey,keyboardSingle:true});await tick();explorer?.querySelector('[data-file-index="'+next+'"]')?.focus();
  }
  function togglePin(){
    if(!root||trash||scope!=='folder')return;
    pins=pinned?pins.filter((pin)=>pin.root!==root||pin.path!==path):[...pins,{root,path,name:title}].slice(-32);
    try{localStorage.setItem('nasctl-file-pins-v1',JSON.stringify(pins));}catch{/* Storage may be unavailable. */}
  }
  function changeView(value){view=value;try{localStorage.setItem('nasctl-files-view',value);}catch{/* Optional preference. */}}
  function operate(operation,entry=null){
    menu=null;message='';const rootID=entry?._root||root;action={operation,entry,root:rootID,directory:entry?.path?('/'+relative(entry.path).split('/').slice(0,-1).join('/')):path};
  }
  // Drag and drop: files dropped on the page upload into the current folder.
  const canDrop=()=>canWrite&&!trash&&scope==='folder';
  function dragOver(event){if(!event.dataTransfer?.types?.includes('Files'))return;event.preventDefault();event.dataTransfer.dropEffect=canDrop()?'copy':'none';dragging=true;}
  function dragLeave(event){if(!event.currentTarget.contains(event.relatedTarget))dragging=false;}
  function drop(event){
    if(!event.dataTransfer?.types?.includes('Files'))return;
    event.preventDefault();dragging=false;if(!canDrop()||action)return;
    const items=[...(event.dataTransfer.items||[])];
    const folders=items.filter((item)=>item.webkitGetAsEntry?.()?.isDirectory).length;
    const dropped=[...event.dataTransfer.files].filter((_,i)=>!items[i]?.webkitGetAsEntry?.()?.isDirectory);
    if(!dropped.length){message=folders?'暂不支持拖放文件夹，请拖入文件，或先新建文件夹。':'';return;}
    menu=null;message=folders?`已跳过 ${folders} 个文件夹，只上传文件。`:'';action={operation:'upload',root,directory:path,files:dropped};
  }
  function operateBatch(operation){menu=null;message='';action={batch:true,operation,items:[...selection],directory:scope==='folder'&&!trash?path:'/'};}
  function toggleTrash(item,event){select(item,-1,{ctrlKey:true});}
  function updateTask(data){
    if(data.job?.id!==task)return;
    for(const event of [...(data.events||[])].reverse()){
      if(!event.note?.startsWith('{'))continue;
      try{const value=JSON.parse(event.note);if(value.kind==='files.batch'&&Array.isArray(value.result?.items)){batchResult=value.result;break;}}catch{/* Other task notes are plain text. */}
    }
  }
  async function toggleLocations(){
    locations=!locations;await tick();
    (locations?explorer?.querySelector('.finder-sidebar button'):explorer?.querySelector('.finder-sidebar-toggle'))?.focus();
  }
  function drawerKeys(event){
    if(explorer?.contains(event.target)&&!event.target.closest('input,textarea,select,[contenteditable="true"],[role="dialog"]')&&(event.metaKey||event.ctrlKey)&&event.key.toLowerCase()==='a'){event.preventDefault();selectAll();return;}
    if(!locations||!explorer?.contains(event.target))return;
    if(event.key==='Escape'){event.preventDefault();toggleLocations();return;}
    if(event.key!=='Tab'||getComputedStyle(explorer.querySelector('.finder-sidebar-toggle')).display==='none')return;
    const nodes=[...explorer.querySelectorAll('.finder-sidebar button:not(:disabled)')];
    if(event.shiftKey&&document.activeElement===nodes[0]){event.preventDefault();nodes.at(-1)?.focus();}
    else if(!event.shiftKey&&document.activeElement===nodes.at(-1)){event.preventDefault();nodes[0]?.focus();}
  }
  function completed(text,jobID=''){action=null;message=text;if(jobID){task=jobID;batchResult=null;}clearSelection();load();}
  async function showMenu(event,entry=selected){
    if(!entry)return;event.preventDefault();event.stopPropagation();if(selected!==entry)select(entry);
    const rect=explorer.getBoundingClientRect(),anchor=event.currentTarget.getBoundingClientRect();
    const x=event.type==='contextmenu'?event.clientX:anchor.left,y=event.type==='contextmenu'?event.clientY:anchor.bottom;
    menu={entry,x:Math.max(8,Math.min(x-rect.left,rect.width-194)),y:Math.max(8,Math.min(y-rect.top,rect.height-295))};await tick();menuElement?.querySelector('button')?.focus();
  }
  async function closeMenu(){menu=null;await tick();explorer?.querySelector('[data-file-index="'+focusIndex+'"]')?.focus();}
  function menuKeys(event){if(event.key==='Escape'){event.preventDefault();closeMenu();}if(!['ArrowUp','ArrowDown','Home','End'].includes(event.key))return;event.preventDefault();const nodes=[...menuElement.querySelectorAll('button:not(:disabled),a[href]')];let index=nodes.indexOf(document.activeElement);index=event.key==='Home'?0:event.key==='End'?nodes.length-1:(index+(event.key==='ArrowDown'?1:-1)+nodes.length)%nodes.length;nodes[index]?.focus();}
  $effect(()=>{if(!grid||typeof ResizeObserver==='undefined')return;const observer=new ResizeObserver(([entry])=>columns=Math.max(1,[...grid.children].filter((node)=>node.offsetTop===grid.children[0]?.offsetTop).length));observer.observe(grid);return()=>observer.disconnect();});
  $effect(()=>{const request=searchRequest;if(!request||!roots.length||request.id===lastSearch)return;untrack(()=>{lastSearch=request.id;go({root:request.root||root,path:'/',search:request.q,appliedSearch:request.q});});});
  onMount(()=>{
    try{view=localStorage.getItem('nasctl-files-view')==='list'?'list':'grid';const values=JSON.parse(localStorage.getItem('nasctl-file-pins-v1')||'[]');if(Array.isArray(values))pins=values.filter((item)=>typeof item.root==='string'&&typeof item.name==='string'&&typeof item.path==='string'&&item.path.length<=1024&&!item.path.includes('\0')&&!item.path.split('/').includes('..')).slice(0,32);}catch{/* Ignore invalid preferences. */}
    const init=new AbortController();api('/api/v1/files/roots',{signal:init.signal}).then((data)=>{if(disposed)return;roots=data.roots||[];root=roots.find((item)=>item.available)?.id||roots[0]?.id||'';uploadMax=data.upload_max_bytes||uploadMax;limits={...limits,...data.limits};storeTab();if(root)load();else loading=false;}).catch((failure)=>{if(!disposed&&failure.name!=='AbortError'){error=failure.message;loading=false;}});
    return()=>{disposed=true;++previewGeneration;clearTimeout(previewTimer);init.abort();controller?.abort();previewRequest?.abort();};
  });
</script>
<svelte:window onkeydown={drawerKeys} onpointerdown={(event)=>{if(menu&&!event.target.closest('.file-context-menu'))menu=null;}}/>
<div class="file-explorer" bind:this={explorer} class:show-details={details} class:show-locations={locations} role="region" aria-label="文件浏览器" ondragenter={dragOver} ondragover={dragOver} ondragleave={dragLeave} ondrop={drop}>
  {#if dragging}<div class="drop-overlay" class:refused={!canDrop()} aria-hidden="true"><Icon name="upload" size={34}/><strong>{canDrop()?'松开上传到“'+(crumbs.at(-1)||rootLabel(root))+'”':trash?'回收站不能上传':'此位置为只读，不能上传'}</strong></div>{/if}
  <div class="finder-tabs"><div class="finder-tab-list" role="tablist" aria-label="文件位置标签">{#each tabs as tab,index (tab.id)}<div class="finder-tab" class:active={tab.id===activeTab}><button role="tab" aria-controls={paneID} tabindex={tab.id===activeTab?0:-1} onkeydown={(event)=>tabKeys(event,index)} aria-selected={tab.id===activeTab} onclick={()=>switchTab(tab)}><Icon name="folder" size={15}/><span>{tabLabel(tab)}</span></button>{#if tabs.length>1}<button class="finder-tab-close" aria-label={'关闭标签 '+tabLabel(tab)} onclick={()=>closeTab(tab)}><Icon name="close" size={12}/></button>{/if}</div>{/each}</div><button class="icon-button" aria-label="新建位置标签" disabled={tabs.length>=8||!root} onclick={addTab}><Icon name="plus" size={17}/></button><span class="finder-workspace">本地空间</span></div>
  <div class="finder-layout" id={paneID} role="tabpanel" aria-label={title}>
    <aside class="finder-sidebar" aria-label="文件位置">
      <div class="finder-sidebar-brand"><span><Icon name="folder" size={19}/></span><div><strong>我的文件</strong><small>你的私人存储</small></div><button class="icon-button finder-sidebar-close" aria-label="收起文件位置" onclick={()=>locations=false}><Icon name="close" size={16}/></button></div>
      <p class="finder-group-label">个人收藏</p>
      <button class="finder-location" class:active={scope==='recent'} onclick={()=>go({scope:'recent'})}><Icon name="clock" size={17}/>最近使用</button>
      <button class="finder-location" class:active={scope==='pinned'} onclick={()=>go({scope:'pinned'})}><Icon name="pin" size={17}/>已固定<span>{pins.length||''}</span></button>
      <p class="finder-group-label">位置</p>
      {#each roots as item}<button class="finder-location" class:active={scope==='folder'&&!trash&&root===item.id} onclick={()=>go({root:item.id,path:'/'})}><Icon name="disk" size={17}/><span class="location-name">{item.label||item.id}</span><span class="status-dot" class:offline={!item.available}></span></button>{/each}
      {#if root&&!agent}<button class="finder-location" class:active={trash} onclick={()=>go({trash:true})}><Icon name="trash" size={17}/>回收站</button>{/if}
      {#if pins.length}<p class="finder-group-label">固定目录</p>{#each pins as pin}<button class="finder-location" disabled={!roots.some((item)=>item.id===pin.root&&item.available)} onclick={()=>go({root:pin.root,path:pin.path})}><Icon name="folder" size={17}/><span class="location-name">{pin.name}</span></button>{/each}{/if}
      <div class="finder-sidebar-foot"><Icon name="shield" size={16}/><span>本地存储 · {canWrite?'允许管理':'只读访问'}</span></div>
    </aside>
    {#if locations}<button class="finder-location-scrim" aria-label="收起文件位置" onclick={()=>locations=false}></button>{/if}
    <div class="finder-main">
      <div class="finder-toolbar">
        <button class="icon-button finder-sidebar-toggle" aria-label="展开文件位置" aria-expanded={locations} onclick={toggleLocations}><Icon name="menu" size={18}/></button>
        <div class="finder-navigation"><button class="icon-button" aria-label="后退" disabled={!currentTab?.back.length} onclick={()=>historyMove('back')}><Icon name="back" size={17}/></button><button class="icon-button" aria-label="前进" disabled={!currentTab?.forward.length} onclick={()=>historyMove('forward')}><Icon name="forward" size={17}/></button><button class="icon-button" aria-label="主目录" disabled={!root} onclick={()=>navigate('/')}><Icon name="home" size={17}/></button></div>
        <nav class="finder-path breadcrumbs" aria-label="文件路径"><button onclick={()=>go({path:'/'})}><Icon name="disk" size={15}/>{rootLabel(root)||'我的文件'}</button>{#if scope!=='folder'||trash}<Icon name="chevron" size={12}/><span>{title}</span>{:else}{#each crumbs as crumb,index}<Icon name="chevron" size={12}/><button title={crumb} onclick={()=>navigate('/'+crumbs.slice(0,index+1).join('/'))}>{crumb}</button>{/each}{/if}</nav>
        <div class="finder-view-toggle" role="group" aria-label="文件视图"><button class:active={view==='grid'} aria-label="图标视图" aria-pressed={view==='grid'} onclick={()=>changeView('grid')}><Icon name="grid" size={17}/></button><button class:active={view==='list'} aria-label="列表视图" aria-pressed={view==='list'} onclick={()=>changeView('list')}><Icon name="list" size={18}/></button></div>
        <button class="icon-button" class:active={details} aria-label={details?'隐藏详情栏':'显示详情栏'} onclick={()=>details=!details}><Icon name="panel" size={18}/></button>
        <button class="icon-button" aria-label="刷新文件" disabled={loading} onclick={()=>{clearSelection();load();}}><Icon name="refresh" size={17}/></button>
      </div>
      <div class="finder-heading"><div><h1>{title}</h1><span>{scope==='recent'?'本次会话打开的项目':scope==='pinned'?'你固定的目录':trash?rootLabel(root)+' 的回收内容':selectedRoot?.available===false?'存储目录暂不可用':canWrite?'可管理的存储目录':'只读浏览'}</span></div><form class="finder-search" onsubmit={(event)=>{event.preventDefault();go({search,appliedSearch:search.trim()});}}><Icon name="search" size={16}/><input type="search" aria-label="搜索文件" placeholder="搜索当前目录及子目录" maxlength="128" bind:value={search}/><button class="sr-only" type="submit">搜索</button></form></div>
      <div class="finder-actionbar"><div class="finder-selected-actions">
        <button class="finder-action" aria-pressed={multiMode} onclick={()=>multiMode=!multiMode}>多选</button>
        {#if multiMode||selection.length>1}<button class="finder-action" disabled={loading||(!entries.length&&!listing?.items?.length)} onclick={selectAll}>选择本页</button>{/if}
        {#if selection.length>1 || trash&&selection.length}
          <span class="field-help">已选 {selection.length} 项</span>
          {#if trash}<button class="finder-action" disabled={!selectionWritable} onclick={()=>operateBatch('restore')}>批量恢复</button>
          {:else}<button class="finder-action" disabled={!canCopy} onclick={()=>operateBatch('copy')}>批量复制</button><button class="finder-action" disabled={!selectionWritable} onclick={()=>operateBatch('move')}>批量移动</button><button class="finder-action danger-text" disabled={!selectionWritable} onclick={()=>operateBatch('trash')}>批量回收</button>{/if}
        {:else if selected&&!trash}<button class="finder-action" onclick={()=>open(selected)}><Icon name={selected.is_dir?'folder':'external'} size={15}/>打开</button><button class="finder-action" aria-label="所选文件更多操作" onclick={(event)=>showMenu(event)}><Icon name="more" size={17}/>更多</button>{:else}<button class="finder-action" disabled={!canWrite||trash||scope!=='folder'} title={!canWrite?'此位置为只读，无法新建':undefined} onclick={()=>operate('mkdir')}><Icon name="folderplus" size={16}/>新建文件夹</button><button class="finder-action" disabled={!canWrite||trash||scope!=='folder'} title={!canWrite?'此位置为只读，无法上传':undefined} onclick={()=>operate('upload')}><Icon name="upload" size={16}/>上传文件</button>{/if}{#if selection.length}<button class="icon-button" aria-label="取消选择" onclick={clearSelection}><Icon name="close" size={14}/></button>{/if}</div><div class="finder-sort">{#if scope==='folder'&&!trash}<button class="icon-button" class:active={pinned} aria-label={pinned?'取消固定当前目录':'固定当前目录'} disabled={!root} onclick={togglePin}><Icon name="pin" size={15}/></button>{/if}<select aria-label="文件排序" bind:value={sort} disabled={scope!=='folder'||trash} onchange={()=>{page=1;clearSelection();storeTab();load();}}><option value="name:asc">名称 ↑</option><option value="name:desc">名称 ↓</option><option value="time:desc">最新修改</option><option value="size:desc">体积最大</option><option value="kind:asc">文件类型</option></select></div></div>
      <div class="finder-scroll">
        {#if error}<ErrorNotice message={error} onretry={()=>load()}/>{/if}{#if message}<div class="notice success" role="status">{message}<button class="text-button" aria-label="关闭文件操作提示" onclick={()=>message=''}>×</button></div>{/if}
        {#if task}{#key task}<section class="task-panel"><TaskMonitor id={task} onupdate={updateTask} onsettled={()=>{clearSelection();load();}}/>{#if batchResult}<FileBatchResult result={batchResult}/>{/if}<a class="text-button" href="#admin/jobs">在任务中心查看</a></section>{/key}{/if}
        {#if loading}<div class="loading-state" role="status">正在读取文件…</div>
        {:else if !roots.length}<section class="empty-state"><Icon name="folder" size={40}/><h2>添加第一个存储目录</h2><p>配置存储目录后，即可在这里浏览和整理文件。</p></section>
        {:else if trash}
          {#if !listing?.items?.length}<div class="empty-state"><Icon name="trash" size={34}/><h2>回收站是空的</h2><p>移入回收站的文件和文件夹会保存在这里。</p></div>{:else}<div class="table-wrap finder-trash"><table><thead><tr><th>选择</th><th>名称</th><th>原位置</th><th>移入时间</th><th>操作</th></tr></thead><tbody>{#each listing.items as item}<tr><td><input type="checkbox" aria-label={'选择回收项 '+item.name} checked={isSelected(item)} onchange={(event)=>toggleTrash(item,event)}/></td><td><strong>{item.name}</strong>{#if item.purge_started}<small class="inline-error">清理中断，仅保留剩余内容</small>{/if}{#if item.recovery_required}<small class="inline-error">需要指定恢复位置</small>{/if}</td><td>{item.original_path||'恢复信息不可用'}</td><td>{time(item.deleted_at)}</td><td><button class="text-button" disabled={!canWrite} onclick={()=>operate('restore',item)}>恢复</button><button class="text-button danger-text" disabled={!canWrite} onclick={()=>operate('purge',item)}>永久删除</button></td></tr>{/each}</tbody></table></div>{/if}
        {:else if !entries.length}<div class="empty-state"><Icon name={searching?'search':scope==='pinned'?'pin':scope==='recent'?'clock':'folder'} size={34}/><h2>{searching?'没有找到匹配文件':scope==='pinned'?'还没有固定目录':scope==='recent'?'还没有最近使用的项目':'这个文件夹是空的'}</h2><p>{scope==='pinned'?'打开一个目录，点击图钉即可固定。':scope==='recent'?'在这里打开的文件会出现在最近使用中。':searching?'换个关键词试试。':'这里暂时没有可显示的文件。'}</p></div>
        {:else}
          {#if view==='list'}<div class="finder-list-head"><span>名称</span><span>修改时间</span><span>大小</span><span>类型</span></div>{/if}
          <div class:finder-grid={view==='grid'} class:finder-list={view==='list'} role="listbox" aria-multiselectable="true" aria-label="文件项目" bind:this={grid}>
            {#each entries as entry,index (String(entry._root||root)+entry.path)}<button class="file-name finder-entry" class:entry-dir={entry.is_dir} class:selected={isSelected(entry)} role="option" aria-selected={isSelected(entry)} tabindex={focusIndex===index?0:-1} data-file-index={index} title={entry.name} onclick={(event)=>select(entry,index,event)} ondblclick={()=>open(entry)} onkeydown={(event)=>fileKeys(event,index)} oncontextmenu={(event)=>showMenu(event,entry)}><FileGlyph {entry} size={view==='grid'?56:25}/><span class="entry-label">{entry.name}</span>{#if entry.symlink}<span class="finder-symlink">↗</span>{/if}{#if view==='grid'}<small>{entry.is_dir?'文件夹':bytes(entry.size)}</small>{:else}<span class="finder-entry-time">{entry.mod_time?time(entry.mod_time):'—'}</span><span class="finder-entry-size">{entry.is_dir?'—':bytes(entry.size)}</span><span class="finder-entry-kind">{kindLabel(entry)}</span>{/if}</button>{/each}
          </div>
        {/if}
      </div>
      <footer class="finder-status"><span>{selection.length?'已选择 '+selection.length+' 项'+(selected&&!selected.is_dir?' · '+bytes(selected.size):''):scope!=='folder'?entries.length+' 项':searching?entries.length+' 个匹配项目':(listing?.total||0)+' 项'}{listing?.truncated?'或更多 · 已达读取上限':''}</span>{#if scope==='folder'&&!searching}<div><button aria-label="上一页文件" disabled={loading||page<=1} onclick={()=>{page--;clearSelection();storeTab();load();}}><Icon name="back" size={14}/></button><span>{page} / {Math.max(1,listing?.pages||Math.ceil((listing?.total||0)/pageSize)||1)}</span><button aria-label="下一页文件" disabled={loading||page>=(listing?.pages||Math.ceil((listing?.total||0)/pageSize)||1)} onclick={()=>{page++;clearSelection();storeTab();load();}}><Icon name="forward" size={14}/></button></div>{/if}<span>{scope==='folder'&&!trash?selectedRoot?.capacity?bytes(selectedRoot.capacity.available_bytes)+' 可用':searching?'最多 '+limits.search_max_results+' 个匹配项':'每页最多 '+pageSize+' 项':''}</span></footer>
    </div>
    {#if details}<aside class="finder-details" aria-label="文件详情"><header><strong>预览与信息</strong><button class="icon-button" aria-label="收起预览详情" onclick={()=>details=false}><Icon name="panel" size={17}/></button></header>{#if selection.length>1}<div class="finder-preview-empty"><Icon name="folder" size={38}/><p>已选择 {selection.length} 个项目</p><small>使用上方批量操作整理文件</small></div>{:else if selected}<div class="finder-details-content">{#if !fullPreview}<FilePreview entry={selected} info={previewInfo} error={previewError} src={previewURL} onerror={()=>previewError='当前格式无法预览，可下载后使用本地应用打开。'}/>{/if}<h2>{selected.name}</h2><span class="finder-detail-type">{kindLabel(selected)}</span><dl><dt>位置</dt><dd>{rootLabel(selectedID)} / {relative(selected.path)}</dd>{#if !selected.is_dir}<dt>大小</dt><dd>{bytes(selected.size)}</dd>{/if}{#if selected.mod_time}<dt>修改时间</dt><dd>{time(selected.mod_time)}</dd>{/if}{#if previewInfo?.entry?.mode}<dt>权限</dt><dd>{previewInfo.entry.mode}</dd>{/if}</dl><button class="button secondary small" onclick={()=>open(selected)}>{selected.is_dir?'打开文件夹':'打开预览'}</button>{#if !selected.is_dir}<a class="button secondary small" href={download(selected)} download={selected.name}><Icon name="download" size={15}/>下载文件</a>{:else if !trash}<a class="button secondary small" href={archive(selected)} download={selected.name+'.zip'} title="打包为 ZIP 下载，不压缩"><Icon name="download" size={15}/>打包下载</a>{/if}</div>{:else}<div class="finder-preview-empty"><Icon name="panel" size={38}/><p>选择一个项目以预览</p><small>单击选择 · 双击打开</small></div>{/if}</aside>{/if}
  </div>
  {#if menu}<div class="file-context-menu" style:left={menu.x+'px'} style:top={menu.y+'px'} role="menu" aria-label="文件操作" tabindex="-1" bind:this={menuElement} onkeydown={menuKeys}><button role="menuitem" onclick={()=>{open(menu.entry);menu=null;}}><Icon name="external" size={15}/>打开</button>{#if !menu.entry.is_dir}<a role="menuitem" href={download(menu.entry)} download={menu.entry.name} onclick={()=>menu=null}><Icon name="download" size={15}/>下载</a>{:else if !trash}<a role="menuitem" href={archive(menu.entry)} download={menu.entry.name+'.zip'} onclick={()=>menu=null}><Icon name="download" size={15}/>打包下载</a>{/if}<hr/><button role="menuitem" disabled={!canCopy} onclick={()=>operate('copy',menu.entry)}><Icon name="copy" size={15}/>复制</button><button role="menuitem" disabled={!selectedWritable} onclick={()=>operate('move',menu.entry)}><Icon name="move" size={15}/>移动</button><button role="menuitem" disabled={!selectedWritable} onclick={()=>operate('rename',menu.entry)}><Icon name="edit" size={15}/>改名／移动</button><hr/><button role="menuitem" class="danger-text" disabled={!selectedWritable} onclick={()=>operate('trash',menu.entry)}><Icon name="trash" size={15}/>移入回收站</button>{#if !selectedWritable}<p>此位置只读，整理操作不可用</p>{/if}</div>{/if}
</div>
{#if action?.batch}<FileBatch operation={action.operation} items={action.items} {roots} directory={action.directory} {limits} onclose={()=>action=null} oncomplete={completed}/>{:else if action}<FileOperation operation={action.operation} initialFiles={action.files||[]} entry={action.entry} root={action.root} {roots} rootLabel={rootLabel(action.root)} directory={action.directory} {uploadMax} {limits} onclose={()=>{action=null;clearSelection();load();}} oncomplete={completed}/>{/if}
{#if fullPreview&&selected}<Dialog title={selected.name} wide onclose={()=>fullPreview=false}><FilePreview entry={selected} info={previewInfo} error={previewError} src={previewURL} onerror={()=>previewError='当前格式无法预览，可下载后使用本地应用打开。'}/>{#if previewInfo&&!previewInfo.previewable}<p class="muted">这种格式可下载后使用本地应用打开。</p>{/if}<footer class="dialog-footer"><span class="muted">{bytes(selected.size)}</span><button class="button secondary" onclick={()=>fullPreview=false}>关闭</button><a class="button primary" href={download(selected)} download={selected.name}>下载文件</a></footer></Dialog>{/if}
