<script>
  import ErrorNotice from '../components/ErrorNotice.svelte';
  import { getContext, onMount } from 'svelte';
  const windowVisible = getContext('nasctl-window-visible') || (() => true);
  import { api, query } from '../lib/api.js';
  import { time, jobError, jobLabels, opLabel } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import Dialog from '../components/Dialog.svelte';
  import Pager from '../components/Pager.svelte';
  import TaskMonitor from '../components/TaskMonitor.svelte';
  import FileBatchResult from '../components/FileBatchResult.svelte';
  import BackupResult from '../components/BackupResult.svelte';
  function backupNote(note){try{const value=JSON.parse(note);if(value.kind==='backup.result'&&value.result)return value.result;}catch{/* Plain task note. */}return null;}
  const backupResult=$derived(detail?.events?.map((event)=>backupNote(event.note)).filter(Boolean).at(-1));
  function batchNote(note){try{const value=JSON.parse(note);if(value.kind==='files.batch'&&Array.isArray(value.result?.items))return value.result;}catch{/* Plain task notes. */}return null;}
  const batchResult=$derived(detail?.events?.map((event)=>batchNote(event.note)).filter(Boolean).at(-1));
  let rows = $state([]), filter = $state('open'), offset = $state(0), loading = $state(true), error = $state(''), detail = $state(null), detailError = $state(''), selected = $state(null);
  let request, detailRequest, generation = 0, disposed = false;
  async function load() {
    const current = ++generation; request?.abort(); request = new AbortController(); loading = true;
    try { const data = await api(query('/api/v1/jobs',{limit:50,offset,open:filter === 'open' ? 'true' : undefined,state:filter === 'open' || filter === 'all' ? undefined : filter}),{signal:request.signal}); if (!disposed && current === generation) { rows = data.jobs || []; error = ''; } }
    catch (failure) { if (!disposed && current === generation && failure.name !== 'AbortError') error = failure.message; }
    finally { if (!disposed && current === generation) loading = false; }
  }
  async function show(job) {
    selected = job; detail = null; detailError = ''; detailRequest?.abort(); detailRequest = new AbortController();
    try { const data = await api('/api/v1/jobs/' + encodeURIComponent(job.id),{signal:detailRequest.signal}); if (selected?.id === job.id) detail = data; }
    catch (failure) { if (!disposed && failure.name !== 'AbortError') detailError = failure.message; }
  }
  function close() { selected = null; detailRequest?.abort(); }
  onMount(() => { load(); const interval = setInterval(() => { if (!document.hidden && windowVisible()) load(); },5000); return () => { disposed = true; request?.abort(); detailRequest?.abort(); clearInterval(interval); }; });
</script>
<header class="page-heading"><div><h1>任务中心</h1><p class="muted">每一次刷新与处理，都有迹可循。</p></div><button class="button secondary" disabled={loading} onclick={load}><Icon name="refresh" size={16}/>刷新</button></header>
{#if error}<ErrorNotice message={error}/>{/if}
<section class="panel"><div class="panel-heading"><h2>任务记录</h2><select aria-label="任务状态" bind:value={filter} onchange={() => {offset = 0; load();}}><option value="open">进行中</option><option value="all">全部任务</option>{#each Object.entries(jobLabels) as [value,label]}<option {value}>{label}</option>{/each}</select></div>{#if loading && !rows.length}<div class="loading-state" role="status">正在读取任务…</div>{:else if !rows.length}<div class="empty-state"><Icon name="jobs" size={38}/><h2>暂时没有{filter === 'open' ? '进行中的' : '匹配的'}任务</h2><p>新的下载与刷新记录会显示在这里。</p></div>{:else}<div class="table-wrap"><table><thead><tr><th>任务</th><th>状态</th><th>发起者</th><th>时间</th><th><span class="sr-only">详情</span></th></tr></thead><tbody>{#each rows as job (job.id)}<tr><td><strong title={job.op}>{opLabel(job.op)}</strong><small class="muted mono">{job.id}</small></td><td><span class="badge" class:complete={job.state === 'completed'} class:failed={['failed','interrupted','unknown'].includes(job.state)}>{jobLabels[job.state] || job.state}</span></td><td>{job.principal}</td><td class="muted">{time(job.created_at)}</td><td><button class="text-button" onclick={() => show(job)}>查看<Icon name="chevron" size={14}/></button></td></tr>{/each}</tbody></table></div>{/if}<Pager {offset} count={rows.length} busy={loading} onchange={(next) => {offset = next; load();}}/></section>
{#if selected}<Dialog title="任务详情" wide onclose={close}>{#if detailError}<ErrorNotice message={detailError} alert={false}/>{:else if !detail}<p role="status">正在读取任务详情…</p>{:else}<TaskMonitor id={selected.id} onupdate={(data) => detail = data} onsettled={() => load()}/><dl class="key-values"><dt>操作</dt><dd>{opLabel(detail.job.op)}{#if opLabel(detail.job.op) !== detail.job.op} <span class="muted">{detail.job.op}</span>{/if}</dd><dt>状态</dt><dd>{jobLabels[detail.job.state] || detail.job.state}</dd><dt>发起者</dt><dd>{detail.job.principal}</dd><dt>创建时间</dt><dd>{time(detail.job.created_at)}</dd>{#if detail.job.error}<dt>错误</dt><dd class="inline-error" style:white-space="pre-line">{jobError(detail.job.error)}</dd>{/if}{#if detail.job.stop_reason}<dt>停止原因</dt><dd>{detail.job.stop_reason}</dd>{/if}</dl>{#if batchResult}<FileBatchResult result={batchResult}/>{/if}{#if backupResult}<BackupResult result={backupResult} operation={detail.job.op} state={detail.job.state}/>{/if}<h3>事件记录</h3><ol class="event-list">{#each detail.events || [] as event}<li><time>{time(event.at)}</time><strong>{event.to || event.from || '记录'}</strong><p class="muted">{batchNote(event.note)?'批量结果已保存，详见上表':backupNote(event.note)?'备份结果已保存，详见上方结果':event.note || '—'}</p></li>{/each}</ol>{/if}</Dialog>{/if}
