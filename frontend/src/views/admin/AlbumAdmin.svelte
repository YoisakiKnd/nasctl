<script>
  import ErrorNotice from '../../components/ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api, json } from '../../lib/api.js';
  import Icon from '../../components/Icon.svelte';
  import TaskMonitor from '../../components/TaskMonitor.svelte';
  let { readOnly = true, canConfigure = true } = $props();
  let status = $state(null), listing = $state(null), configuration = $state(null), roots = $state([]), pending = $state(false), loading = $state(true), controlError = $state(''), controlMessage = $state('');
  let code = $state(''), directoryName = $state(''), directoryRoot = $state(''), directoryPath = $state(''), enabled = $state(true), sharedVolume = $state(false), editing = $state(false), removing = $state('');
  let workCollection = $state(''), workLimit = $state(50), workRuntime = $state('10m'), jobID = $state(''), taskRunning = $state(false);
  let disposed = false;
  function warningText(value) {
    if (value.startsWith('no token is configured:')) return '尚未设置访问令牌，请仅在可信网络中使用。';
    if (status?.mode === 'local' && value.startsWith('a task is recorded as running.')) return '相册处理正在运行，已建立的索引仍可浏览。';
    return value;
  }
  async function load() {
    loading = true; controlError = '';
    try {
      const state = await api('/api/v1/album/status'); if (disposed) return; status = state;
      if (state.mode !== 'local') return;
      const [settings, locations, page] = await Promise.all([api('/api/v1/album/settings'),api('/api/v1/files/roots'),state.connected && state.index_ready !== false ? api('/api/v1/album/media?limit=1') : Promise.resolve(null)]);
      if (disposed) return;
      configuration = settings; roots = locations.roots || []; listing = page;
      directoryRoot ||= roots.find((item) => item.available)?.id || ''; workCollection ||= settings.directories.find((item) => item.enabled)?.code || '';
    } catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) loading = false;}
  }
  function resetDirectory() {code = ''; directoryName = ''; directoryPath = ''; enabled = true; sharedVolume = false; editing = false; removing = '';}
  function editDirectory(item) {code = item.code; directoryName = item.name; directoryRoot = item.root; directoryPath = item.path; enabled = item.enabled; sharedVolume = item.allow_shared_volume; editing = true; removing = '';}
  async function saveDirectory(event) {
    event.preventDefault(); if (pending||!canConfigure) return; pending = true; controlError = ''; controlMessage = '';
    try {const saved = await api('/api/v1/album/directories',json('POST',{code,name:directoryName,root:directoryRoot,path:directoryPath,enabled,allow_shared_volume:sharedVolume},{headers:{'X-Nasctl-Confirm':'1'}})); if (disposed) return;
      configuration = saved; workCollection ||= code; resetDirectory(); controlMessage = '目录已保存，已有照片不会被移动。可以开始一次限量扫描。';
    } catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) pending = false;}
  }
  async function removeDirectory(value) {
    if (pending||!canConfigure) return; pending = true; controlError = '';
    try {const saved = await api('/api/v1/album/directories/' + encodeURIComponent(value),{method:'DELETE',headers:{'X-Nasctl-Confirm':'1'}}); if (disposed) return; configuration = saved; resetDirectory(); workCollection = saved.directories.find((item) => item.enabled)?.code || ''; controlMessage = '目录配置已移除，原文件与已有索引保留。';}
    catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) pending = false;}
  }
  async function startWork(operation) {
    if (pending || taskRunning) return; pending = true; controlError = ''; controlMessage = '';
    const input = ['scan','thumbs'].includes(operation) ? {collection:workCollection,limit:workLimit,max_runtime:workRuntime} : operation === 'pause' ? {reason:'由相册操作者暂停'} : {};
    try {const accepted = await api('/api/v1/album/tasks/' + operation,json('POST',input,{headers:{'X-Nasctl-Confirm':'1'}})); if (disposed) return; jobID = accepted.job_id; taskRunning = true;}
    catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) pending = false;}
  }
  onMount(() => {load(); return () => {disposed = true;};});
</script>
<header class="page-heading"><div><h1>相册管理</h1><p class="muted">照片目录与限量处理任务。浏览照片请前往<a class="text-button" href="#album">相册</a>。</p></div><button class="button secondary" disabled={loading || pending} onclick={load}><Icon name="refresh" size={16}/>刷新</button></header>
<div class="album-admin">
{#if jobID}<section class="panel task-panel"><TaskMonitor id={jobID} onsettled={() => {taskRunning = false; load();}}/><a class="text-button" href="#admin/jobs">在任务中心查看</a></section>{/if}
{#if controlMessage}<p class="notice success" role="status">{controlMessage}</p>{/if}
{#if loading && !status}<p class="loading-state" role="status">正在读取相册设置…</p>
{:else if status && !status.configured}<section class="empty-state panel"><Icon name="album" size={40}/><h2>相册已停用</h2><p>在配置文件中将 hub.album.mode 设为 local 后即可使用。</p></section>
{:else if status && status.mode !== 'local'}<p class="notice warning">当前相册不是本地模式，无法管理目录。</p>
{:else}
  {#if status?.paused}<p class="notice warning">相册处理已暂停{status.pause_reason ? '：' + status.pause_reason : ''}。已有照片仍可浏览。</p>{/if}
  {#each [...(status?.warnings || []),...(listing?.warnings || [])] as warning}<p class="notice warning">{warningText(warning)}</p>{/each}
  {#if listing?.degraded}<p class="notice warning">缩略图处理处于资源保护状态，仍可读取已有内容。</p>{/if}
  {#if controlError}<ErrorNotice message={controlError}/>{/if}
  {#if (pending || loading) && !configuration}<p role="status">正在读取目录设置…</p>{/if}
  {#if configuration}
    <section class="panel album-control-section"><h3>照片目录</h3><p class="muted">从已配置的存储位置添加目录。原文件保持原位，扫描只更新索引。</p>
      {#each configuration.directories as item}<div class="album-directory-row"><div><strong>{item.name}</strong><small class="muted">{item.code} · {item.root ? item.root + '/' : ''}{item.path || '根目录'} · {item.enabled ? '已启用' : '已停用'}</small></div>{#if item.managed&&canConfigure}<div class="button-group"><button class="text-button" disabled={pending || taskRunning || configuration.busy} onclick={() => editDirectory(item)}>编辑</button><button class="text-button" disabled={pending || taskRunning || configuration.busy} onclick={() => removing = item.code}>移除</button></div>{:else}<small class="muted">{item.managed?'管理员配置':'主配置文件'}</small>{/if}</div>{/each}
      {#if removing}<div class="notice"><p>移除 {removing} 的目录配置？原文件与已有索引保留。</p><div class="button-group"><button class="button secondary small" onclick={() => removing = ''}>取消</button><button class="button primary small" disabled={pending} onclick={() => removeDirectory(removing)}>确认移除目录配置</button></div></div>{/if}
      {#if canConfigure}<form class="album-directory-form" onsubmit={saveDirectory}>
        <label>集合代码<input aria-label="相册集合代码" bind:value={code} required maxlength="32" pattern={'[a-z0-9][a-z0-9_\\-]{0,31}'} disabled={editing} placeholder="photos"/></label><label>名称<input aria-label="相册目录名称" bind:value={directoryName} required maxlength="128" placeholder="家庭照片"/></label>
        <label>存储位置<select aria-label="相册存储位置" bind:value={directoryRoot} required><option value="" disabled>选择存储位置</option>{#each roots as root}<option value={root.id} disabled={!root.available}>{root.label || root.id}{!root.available ? '（不可用）' : ''}</option>{/each}</select></label><label>相对目录<input aria-label="相册相对目录" bind:value={directoryPath} maxlength="4096" placeholder="照片（留空使用根目录）"/></label>
        <label class="check-row"><input type="checkbox" bind:checked={enabled}/><span>启用此集合</span></label><label class="check-row"><input type="checkbox" bind:checked={sharedVolume}/><span>允许照片与系统位于同一卷</span></label>
        <p class="field-help">同卷选项适用于单盘设备；独立数据盘请保持关闭，以便检测挂载异常。</p><div class="button-group"><button class="button primary" type="submit" disabled={pending || taskRunning || configuration.busy || !directoryRoot}>{pending ? '正在保存…' : editing ? '确认保存目录' : '确认添加目录'}</button>{#if editing}<button type="button" class="button secondary" onclick={resetDirectory}>取消编辑</button>{/if}</div>
      </form>{:else}<p class="field-help">目录配置由管理员管理，下方可执行限量任务。</p>{/if}
    </section>
    <section class="panel album-control-section"><h3>限量处理</h3>{#if !configuration.worker_available}<p class="notice warning">相册处理程序不可用。请检查 nasctl 可执行文件或 hub.album.worker_binary 配置。</p>{/if}
      <div class="album-directory-form"><label>处理集合<select aria-label="相册处理集合" bind:value={workCollection}><option value="" disabled>选择集合</option>{#each configuration.directories.filter((item) => item.enabled) as item}<option value={item.code}>{item.name}</option>{/each}</select></label><label>每次最多处理<input aria-label="相册任务限额" type="number" min="1" max="10000" bind:value={workLimit}/></label><label>最长运行时间<input aria-label="相册任务时限" bind:value={workRuntime} maxlength="16" placeholder="10m"/></label></div>
      <p class="field-help">扫描先建立索引，再按需生成缩略图。任务受运行模式、内存和挂载检查限制；进度与结果可在任务中心查看。</p><div class="button-group"><button class="button primary" disabled={pending || taskRunning || configuration.busy || !configuration.worker_available || !workCollection} onclick={() => startWork('scan')}>确认限量扫描</button><button class="button secondary" disabled={pending || taskRunning || configuration.busy || !configuration.worker_available || !workCollection} onclick={() => startWork('thumbs')}>确认生成缩略图</button><button class="button secondary" disabled={pending || taskRunning || configuration.busy || !configuration.worker_available} onclick={() => startWork(status?.paused ? 'resume' : 'pause')}>{status?.paused ? '恢复处理许可' : '暂停后续处理'}</button></div>
    </section>
  {/if}
{/if}
{#if readOnly}<p class="notice warning">控制台当前为只读模式，目录和任务操作会被拒绝。</p>{/if}
</div>
