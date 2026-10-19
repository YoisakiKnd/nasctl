<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { onDestroy, untrack } from 'svelte';
  import { api, json, query } from '../lib/api.js';
  import { uploadFile } from '../lib/upload.js';
  import { bytes } from '../lib/format.js';
  import Dialog from './Dialog.svelte';
  let { operation, root, roots = [], rootLabel, directory = '/', entry = null, uploadMax = 8 * 1024 ** 3, limits = {copy_max_entries:10000,copy_max_bytes:8*1024**3,batch_max_items:100}, initialFiles = [], onclose, oncomplete } = $props();
  const initial = untrack(() => ({ operation, entry, root, roots }));
  function copyName(entry) {const path = entry.path.replace(/^\/+/, ''); const dot = path.lastIndexOf('.'); return !entry.is_dir && dot > path.lastIndexOf('/') + 1 ? path.slice(0,dot) + '（副本）' + path.slice(dot) : path + '（副本）';}
  let destination = $state(['rename','move'].includes(initial.operation) ? initial.entry.path.replace(/^\/+/, '') : initial.operation === 'copy' ? copyName(initial.entry) : initial.operation === 'restore' ? initial.entry.original_path || '' : initial.operation === 'newfile' ? '新建文本.txt' : '');
  let destinationRoot = $state(initial.roots.find((item) => item.id === initial.root && item.writable)?.id || initial.roots.find((item) => item.writable && item.available)?.id || '');
  let confirmation = $state('');
  let files = $state([]), pending = $state(false), error = $state(''), progress = $state(0), index = $state(0), stopped = $state(false);
  let controller, disposed = false;
  const labels = { mkdir:'新建文件夹',newfile:'新建文件',rename:'移动或重命名',trash:'移入回收站',restore:'恢复文件',upload:'上传文件',copy:'复制',move:'移动到存储目录',purge:'永久删除' };
  function join(name) { const base = directory.replace(/^\/+|\/+$/g,''); return base ? base + '/' + name : name; }
  function choose(event) { setFiles(Array.from(event.currentTarget.files || [])); }
  // Files dropped onto the file page arrive here preselected.
  function setFiles(list) {
    files = list;
    error = ''; index = 0; stopped = false;
    destination = files.length === 1 ? files[0].name : '';
    if (files.length > limits.batch_max_items) {error = `一次最多选择 ${limits.batch_max_items} 个文件。`; files = [];}
  }
  if (initial.operation === 'upload') untrack(() => { if (initialFiles.length) setFiles([...initialFiles]); });
  function close() { if (!pending) onclose(); }
  async function submit(event) {
    event.preventDefault(); if (pending) return;
    pending = true; error = ''; stopped = false; controller = new AbortController();
    try {
      if (operation === 'upload') {
        for (; index < files.length; index++) {
          const file = files[index];
          if (file.size > uploadMax) throw new Error(`${file.name} 超过 ${bytes(uploadMax)} 上传上限`);
          const name = files.length === 1 ? destination : file.name;
          if (!name || name === '.' || name === '..' || name.includes('/')) throw new Error('请输入有效文件名');
          progress = 0;
          await uploadFile(root,join(name),file,{signal:controller.signal,onprogress:(loaded,total) => {if (!disposed) progress = total ? loaded / total : 0;}});
          if (disposed) return;
        }
      } else if (operation === 'newfile') {
        const name = destination.trim();
        if (!name || name === '.' || name === '..' || name.includes('/')) throw new Error('文件名称不能为空或包含斜线');
        await api(query('/api/v1/files/text',{root,path:join(name)}),{method:'PUT',headers:{'Content-Type':'text/plain; charset=utf-8','X-Nasctl-Confirm':'1'},body:'',signal:controller.signal});
        if (!disposed) oncomplete('已新建“' + name + '”。','',join(name));
        return;
      } else {
        const body = {root};
        if (operation === 'mkdir') {
          if (!destination.trim() || destination.includes('/')) throw new Error('文件夹名称不能为空或包含斜线');
          body.path = join(destination);
        } else if (operation === 'rename') {body.from = entry.path.replace(/^\/+/, ''); body.to = destination;}
        else if (operation === 'trash') body.path = entry.path.replace(/^\/+/, '');
        else if (operation === 'restore') {body.id = entry.id; body.to = destination;}
        else if (operation === 'copy' || operation === 'move') {body.from = entry.path.replace(/^\/+/, ''); body.to = destination; body.to_root = destinationRoot; body.limit = limits.copy_max_entries; body.max_bytes = limits.copy_max_bytes;}
        else if (operation === 'purge') {if (confirmation !== entry.name) throw new Error('请输入完整名称以确认'); body.id = entry.id; body.confirm_name = confirmation; body.limit = limits.copy_max_entries;}
        const result = await api('/api/v1/files/' + operation,json('POST',body,{headers:{'X-Nasctl-Confirm':'1'},signal:controller.signal}));
        if (result.job_id) {if (!disposed) oncomplete(labels[operation] + '任务已创建。',result.job_id); return;}
      }
      if (!disposed) oncomplete(labels[operation] + '已完成。');
    } catch (failure) {
      if (!disposed) {
        stopped = failure.name === 'AbortError';
        error = stopped ? '已请求停止。已完成的文件可能保留，请关闭后刷新确认。' : failure.message;
      }
    } finally {if (!disposed) pending = false;}
  }
  onDestroy(() => {disposed = true; controller?.abort();});
</script>
<Dialog title={labels[operation]} onclose={close}>
  <form class="file-operation" onsubmit={submit}>
    <p class="field-help">存储目录：{rootLabel} · {operation === 'upload' || operation === 'mkdir' || operation === 'newfile' ? directory : entry?.name}</p>
    {#if operation === 'trash'}<p>将“{entry.name}”移入此存储目录的回收站，之后可以恢复。</p>
    {:else if operation === 'purge'}<p class="notice error">永久删除“{entry.name}”及其内容，释放占用空间。删除的内容无法通过回收站恢复；停止任务只能保留尚未删除的部分。</p><label>输入完整名称确认<input aria-label="永久删除确认名称" bind:value={confirmation} required autocomplete="off" disabled={pending}/></label><p class="field-help">本次最多处理 {limits.copy_max_entries.toLocaleString()} 项、32 层目录。超过上限会在删除开始前停止。</p>
    {:else if operation === 'upload'}
      <label>选择文件<input type="file" multiple onchange={choose} disabled={pending || index > 0}/></label>
      {#if files.length === 1}<label>保存名称<input aria-label="上传保存名称" bind:value={destination} required disabled={pending}/></label>{/if}
      {#if files.length}<ul class="upload-files">{#each files as file, i}<li><span>{i < index ? '✓ ' : ''}{file.name}</span><small>{bytes(file.size)}</small></li>{/each}</ul>{/if}
      <p class="field-help">每个文件最多 {bytes(uploadMax)}，按顺序上传，同名文件会提示冲突。</p>
      {#if pending}<div role="status"><progress max="1" value={progress} aria-label="上传进度"></progress><p>{index + 1} / {files.length} · {Math.round(progress * 100)}%{progress >= 1 ? ' · 正在完成保存…' : ''}</p></div>{/if}
    {:else}{#if operation === 'copy' || operation === 'move'}<label>目标存储目录<select aria-label="目标存储目录" bind:value={destinationRoot} disabled={pending}>{#each roots.filter((item) => item.writable && item.available) as item}<option value={item.id}>{item.label || item.id}</option>{/each}</select></label>{/if}<label>{operation === 'mkdir' ? '文件夹名称' : operation === 'newfile' ? '文件名称' : '新位置（相对存储目录）'}<input aria-label="文件操作目标" bind:value={destination} required maxlength="1024" disabled={pending}/></label>{#if operation === 'newfile'}<p class="field-help">新建一个空的文本文件并打开编辑，例如：笔记.md、配置.yaml。</p>{:else if operation !== 'mkdir'}<p class="field-help">例如：照片/旅行.jpg。目标名称已经存在时，需要选择其他名称。</p>{/if}{#if operation === 'copy' || operation === 'move'}<p class="field-help">后台处理，最多 {bytes(limits.copy_max_bytes)}、{limits.copy_max_entries.toLocaleString()} 项和 32 层目录。目标的上级文件夹需已存在。{operation === 'move' ? '跨存储移动成功后，源内容进入原存储目录的回收站，可恢复。' : '保留源内容，完整复制后才显示目标。'}</p>{/if}{/if}
    {#if error}<ErrorNotice message={error}/>{/if}
    <div class="dialog-footer">
      {#if pending && operation === 'upload'}<button class="button secondary" type="button" onclick={() => controller?.abort()}>停止上传</button>{:else}<button class="button secondary" type="button" disabled={pending} onclick={close}>关闭</button>{/if}
      <button class="button primary" type="submit" disabled={pending || (operation === 'upload' && !files.length) || (operation === 'purge' && confirmation !== entry.name)}>{pending ? '正在处理…' : operation === 'upload' ? index > 0 ? '继续上传剩余文件' : '开始上传' : '确认' + labels[operation]}</button>
    </div>
    {#if stopped}<p class="field-help">关闭窗口后刷新文件列表，再决定是否重试。</p>{/if}
  </form>
</Dialog>
