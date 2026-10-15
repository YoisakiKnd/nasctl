<script>
  import { getContext, untrack } from 'svelte';
  const windowVisible = getContext('nasctl-window-visible') || (() => true);
  import { api, json } from '../lib/api.js';
  import { bytes, jobLabels } from '../lib/format.js';
  let { id, onsettled = () => {}, onupdate = () => {} } = $props();
  let job = $state(null), detail = $state(''), error = $state(''), confirming = $state(false), stopping = $state(false);
  let disposed = false, timer, controller, settled = false, generation = 0;
  const active = (value) => ['queued','running','waiting','paused'].includes(value);
  function amount(n) { return job?.unit === 'bytes' ? bytes(n) : `${n} 项`; }
  async function refresh() {
    if (disposed) return;
    if (document.hidden || !windowVisible()) {clearTimeout(timer); timer = setTimeout(refresh,5000); return;}
    clearTimeout(timer); controller?.abort(); controller = new AbortController();
    const current = ++generation;
    try {
      const data = await api('/api/v1/jobs/' + encodeURIComponent(id),{signal:controller.signal});
      if (disposed || current !== generation) return;
      job = data.job; detail = data.events?.filter((event) => event.to === 'running' && event.note).at(-1)?.note || ''; error = ''; onupdate(data);
      if (detail.startsWith('{')) {
        try {const note=JSON.parse(detail);if(note.kind==='files.batch'&&note.result)detail=`完成 ${note.result.completed} 项，失败 ${note.result.failed} 项，未开始 ${note.result.not_started} 项。逐项结果已保存到任务记录。`;else if(note.kind==='backup.result')detail='备份任务结果已保存，可在任务中心重新查看。';}catch{/* Keep a plain task note. */}
      }
      if (active(job.state)) timer = setTimeout(refresh,1500);
      else if (!settled) { settled = true; onsettled(job); }
    } catch (failure) { if (!disposed && current === generation && failure.name !== 'AbortError') {error = failure.message; timer = setTimeout(refresh,5000);} }
  }
  async function cancel() {
    const currentID = id;
    stopping = true;
    try {await api('/api/v1/jobs/' + encodeURIComponent(currentID) + '/cancel',json('POST',{}, {headers:{'X-Nasctl-Confirm':'1'}})); if (!disposed && id === currentID) {confirming = false; refresh();}}
    catch (failure) {if (!disposed && id === currentID) {error = failure.message; stopping = false;}}
  }
  $effect(() => {
    const currentID = id;
    if (!currentID) return;
    disposed = false; settled = false; job = null; detail = ''; error = ''; confirming = false; stopping = false;
    untrack(refresh);
    return () => {disposed = true; generation++; clearTimeout(timer); controller?.abort();};
  });
</script>
<div class="task-monitor" role="status">
  {#if job}<div class="button-group"><strong>{job.op}</strong><span class="badge">{jobLabels[job.state] || job.state}</span>{#if job.cancellable && !stopping}<button class="text-button" onclick={() => confirming = !confirming}>停止任务</button>{/if}</div>
    {#if job.unit}<p class="field-help">已处理 {amount(job.progress)}{job.total > 0 ? ' / ' + amount(job.total) : ' · 总量待完成后确认'}</p>{#if job.total > 0}<progress max={job.total} value={job.progress} aria-label="任务进度"></progress>{/if}{/if}
    {#if detail}<p class="field-help task-detail">{detail}</p>{/if}
    {#if job.error}<p class="inline-error">{job.error}</p>{/if}
    {#if stopping && active(job.state)}<p class="field-help">已请求停止，正在收尾…</p>{/if}
    {#if confirming}<div class="notice"><p>{job.op === 'files.purge' ? '停止后，已删除的内容无法恢复，回收站将保留剩余部分。' : job.op.startsWith('anime.') ? '停止后，已经提交到下载器的任务会继续运行，可到下载器中管理。' : job.op.startsWith('album.') ? '停止后会保留已保存的索引和缩略图；尚未处理的照片可以下次继续。' : '停止后会清理未完成的副本；已经保存的目标会保留，请刷新文件列表核对。'}</p><div class="button-group"><button class="button secondary small" onclick={() => confirming = false}>继续运行</button><button class="button primary small" onclick={cancel}>确认停止任务</button></div></div>{/if}
  {:else}<p class="field-help">正在读取任务状态…</p>{/if}
  {#if error}<p class="inline-error">{error}</p>{/if}
</div>
