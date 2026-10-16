<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { untrack, onDestroy } from 'svelte';
  import { api, json } from '../lib/api.js';
  import { bytes } from '../lib/format.js';
  import Dialog from './Dialog.svelte';
  let { operation, items, roots, directory, limits, onclose, oncomplete } = $props();
  const initial = untrack(() => ({ items, roots, directory, operation, limits }));
  const names = {copy:'复制',move:'移动',trash:'移入回收站',restore:'恢复'};
  let destinationRoot = $state(initial.roots.find((r)=>r.id===initial.items[0]._root&&r.writable&&r.available)?.id || initial.roots.find((r)=>r.writable&&r.available)?.id || '');
  let destination = $state(initial.directory.replace(/^\/+/, '') || '.');
  let limit = $state(initial.limits.copy_max_entries), maxBytes = $state(initial.limits.copy_max_bytes);
  let pending = $state(false), error = $state(''), request;
  async function submit(event) {
    event.preventDefault(); if (pending) return; pending=true; error='';request=new AbortController();
    try {
      const body={action:operation,items:items.map((item)=>operation==='restore'?{root:item._root,id:item.id}:{root:item._root,path:item.path.replace(/^\/+/, '')}),limit:Number(limit),max_bytes:Number(maxBytes)};
      if (operation==='copy'||operation==='move') {body.to_root=destinationRoot;body.directory=destination;}
      const accepted=await api('/api/v1/files/batch',json('POST',body,{headers:{'X-Nasctl-Confirm':'1'},signal:request.signal}));
      oncomplete(`批量${names[operation]}任务已创建。`,accepted.job_id);
    } catch (failure) {if(failure.name!=='AbortError')error=failure.message;}
    finally {pending=false;}
  }
  onDestroy(()=>request?.abort());
</script>
<Dialog title={'批量'+names[operation]} onclose={()=>{if(!pending)onclose();}}>
  <form class="file-operation" onsubmit={submit}>
    <p>已选择 {items.length} 项。将按顺序处理；已完成项目会保留，同名冲突会列出并继续下一项。</p>
    <ul class="upload-files">{#each items as item}<li><span>{item.name}</span><small>{roots.find((r)=>r.id===item._root)?.label||item._root}</small></li>{/each}</ul>
    {#if operation==='copy'||operation==='move'}
      <label>目标存储目录<select aria-label="批量目标存储目录" bind:value={destinationRoot} disabled={pending}>{#each roots.filter((r)=>r.writable&&r.available) as r}<option value={r.id}>{r.label||r.id}</option>{/each}</select></label>
      <label>目标文件夹<input aria-label="批量目标文件夹" bind:value={destination} required maxlength="1024" disabled={pending}/></label>
      <p class="field-help">填 . 表示存储根目录。目标文件夹需已存在，每项保留原名称；不会覆盖现有内容。{operation==='move'?'跨存储移动成功后，源文件进入原存储目录回收站。':''}</p>
    {:else if operation==='trash'}<p>将所选项目放入各自存储目录的回收站，可以恢复。</p>
    {:else}<p>恢复到原位置；位置已占用或恢复信息损坏的项目会单独提示，可使用单项恢复指定其他位置。</p>{/if}
    <label>整批条目预算<input aria-label="批量条目预算" type="number" min="1" max={limits.copy_max_entries} bind:value={limit} required disabled={pending}/></label>
    {#if operation==='copy'||operation==='move'}<label>整批复制字节预算<input aria-label="批量字节预算" type="number" min="1" max={limits.copy_max_bytes} bind:value={maxBytes} required disabled={pending}/></label>{/if}
    <p class="field-help">整批上限 {limits.copy_max_entries.toLocaleString()} 项、{bytes(limits.copy_max_bytes)}；达到预算或停止任务时，剩余项目不再开始。文件夹复制计入全部子项，回收／恢复计为一次移动。</p>
    {#if error}<ErrorNotice message={error}/>{/if}
    <footer class="dialog-footer"><button class="button secondary" type="button" disabled={pending} onclick={onclose}>关闭</button><button class="button primary" type="submit" disabled={pending||!items.length||(operation==='copy'||operation==='move')&&!destinationRoot}>{pending?'正在创建任务…':'确认批量'+names[operation]}</button></footer>
  </form>
</Dialog>
