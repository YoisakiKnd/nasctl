<script>
  import ErrorNotice from '../../components/ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api, json } from '../../lib/api.js';
  import { vault, refreshVault } from '../../lib/vault.svelte.js';
  import Icon from '../../components/Icon.svelte';
  import Dialog from '../../components/Dialog.svelte';
  import TaskMonitor from '../../components/TaskMonitor.svelte';
  import VaultUnlock from '../../components/VaultUnlock.svelte';
  let { readOnly = true, canConfigure = true } = $props();
  let status = $state(null), listing = $state(null), configuration = $state(null), roots = $state([]), pending = $state(false), loading = $state(true), controlError = $state(''), controlMessage = $state(''), editorError = $state('');
  let code = $state(''), directoryName = $state(''), directoryRoot = $state(''), directoryPath = $state(''), enabled = $state(true), sharedVolume = $state(false), privateDirectory = $state(false), wasPrivate = $state(false), editing = $state(false), editorOpen = $state(false), removing = $state(null);
  let workCollection = $state(''), workRuntime = $state('12h'), jobID = $state(''), taskRunning = $state(false), chained = $state(false);
  let disposed = false;
  // The whole directory is processed in batches; the runtime caps one run,
  // and starting again continues with what is left.
  const runtimes = [['12h','直到处理完（最长 12 小时）'],['6h','最长 6 小时'],['2h','最长 2 小时'],['30m','最长 30 分钟']];
  const batchSize = {scan:500, thumbs:50};
  const directories = $derived(configuration?.directories || []);
  const privateCount = $derived(directories.filter((item) => item.private).length);
  const busy = $derived(pending || taskRunning || Boolean(configuration?.busy));
  // Changing anything about a private directory needs the vault password.
  const needsUnlock = $derived((privateDirectory || wasPrivate) && !vault.status?.unlocked);
  function warningText(value) {
    if (value.startsWith('no token is configured:')) return '尚未设置访问令牌，请仅在可信网络中使用。';
    if (status?.mode === 'local' && value.startsWith('a task is recorded as running.')) return '相册处理正在运行，已建立的索引仍可浏览。';
    return value;
  }
  function rootLabel(id) {return roots.find((item) => item.id === id)?.label || id;}
  function location(item) {return item.managed ? rootLabel(item.root) + ' / ' + (item.path === '.' || !item.path ? '根目录' : item.path) : item.path;}
  async function load() {
    loading = true; controlError = '';
    try {
      const state = await api('/api/v1/album/status'); if (disposed) return; status = state;
      if (state.mode !== 'local') return;
      const [settings, locations, page] = await Promise.all([api('/api/v1/album/settings'),api('/api/v1/files/roots'),state.connected && state.index_ready !== false ? api('/api/v1/album/media?limit=1') : Promise.resolve(null)]);
      if (disposed) return;
      configuration = settings; roots = locations.roots || []; listing = page;
      workCollection ||= settings.directories.find((item) => item.enabled)?.code || '';
    } catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) loading = false;}
  }
  function openEditor(item = null) {
    editorError = ''; removing = null; editing = Boolean(item);
    code = item?.code || ''; directoryName = item?.name || ''; directoryRoot = item?.root || roots.find((root) => root.available)?.id || ''; directoryPath = item?.path === '.' ? '' : item?.path || '';
    enabled = item ? item.enabled : true; sharedVolume = Boolean(item?.allow_shared_volume); privateDirectory = Boolean(item?.private); wasPrivate = Boolean(item?.private);
    editorOpen = true; refreshVault();
  }
  function closeEditor() {if (!pending) editorOpen = false;}
  async function saveDirectory(event) {
    event.preventDefault(); if (pending || !canConfigure || needsUnlock) return; pending = true; editorError = ''; controlMessage = '';
    try {
      const saved = await api('/api/v1/album/directories',json('POST',{code,name:directoryName,root:directoryRoot,path:directoryPath,enabled,allow_shared_volume:sharedVolume,private:privateDirectory},{headers:{'X-Nasctl-Confirm':'1'}})); if (disposed) return;
      configuration = saved; workCollection ||= code; editorOpen = false;
      controlMessage = privateDirectory && !wasPrivate ? `「${directoryName}」已设为私密目录：它不再出现在普通相册和文件页里，解锁保险箱后才能查看。` : '目录已保存，已有照片不会被移动。可以开始一次限量扫描。';
      if (privateDirectory || wasPrivate) refreshVault();
    } catch (failure) {if (!disposed) editorError = failure.message;} finally {if (!disposed) pending = false;}
  }
  async function removeDirectory(item) {
    if (pending || !canConfigure) return; pending = true; controlError = '';
    try {const saved = await api('/api/v1/album/directories/' + encodeURIComponent(item.code),{method:'DELETE',headers:{'X-Nasctl-Confirm':'1'}}); if (disposed) return; configuration = saved; removing = null; workCollection = saved.directories.find((dir) => dir.enabled)?.code || ''; controlMessage = '目录配置已移除，原文件与已有索引保留。'; if (item.private) refreshVault();}
    catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) pending = false;}
  }
  async function startWork(operation) {
    if (pending || taskRunning) return; pending = true; controlError = ''; controlMessage = '';
    const input = ['scan','thumbs'].includes(operation) ? {collection:workCollection,limit:batchSize[operation],max_runtime:workRuntime} : operation === 'pause' ? {reason:'由相册操作者暂停'} : {};
    try {const accepted = await api('/api/v1/album/tasks/' + operation,json('POST',input,{headers:{'X-Nasctl-Confirm':'1'}})); if (disposed) return; jobID = accepted.job_id; taskRunning = true;}
    catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) pending = false;}
  }
  onMount(() => {load(); return () => {disposed = true;};});
</script>
<header class="page-heading"><div><h1>相册管理</h1><p class="muted">照片目录、私密目录与相册处理。浏览照片请前往<a class="text-button" href="#album">相册</a>。</p></div><button class="button secondary" disabled={loading || pending} onclick={load}><Icon name="refresh" size={16}/>刷新</button></header>
<div class="album-admin">
{#if jobID}<section class="panel task-panel">{#key jobID}<TaskMonitor id={jobID} onsettled={(job) => {taskRunning = false; load(); if (chained) {chained = false; if (job?.state === 'completed') startWork('thumbs');}}}/>{/key}<a class="text-button" href="#admin/jobs">在任务中心查看</a></section>{/if}
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
    <div class="stat-strip album-stats">
      <div><span>普通相册内容</span><strong>{listing?.total_estimate >= 100000 ? '10 万+' : listing?.total_estimate ?? '—'}<small>项</small></strong></div>
      <div><span>照片目录</span><strong>{directories.length}<small>个</small></strong></div>
      <div><span>私密目录</span><strong>{privateCount}<small>个</small></strong></div>
      <div><span>处理状态</span><strong class="text-stat">{status?.paused ? '已暂停' : configuration.busy || taskRunning ? '处理中' : '空闲'}</strong></div>
    </div>
    <section class="panel settings-card">
      <header class="settings-card-head"><span class="settings-card-icon"><Icon name="albums" size={19}/></span><div><h2>照片目录</h2><p class="muted">从已配置的存储位置添加目录。原文件始终留在原处，扫描只更新索引。</p></div>{#if canConfigure}<button class="button primary small" disabled={busy || !roots.length} onclick={() => openEditor()}><Icon name="plus" size={15}/>添加目录</button>{/if}</header>
      <div class="directory-list">
        {#each directories as item (item.code)}
          <div class="album-directory-row directory-card" class:private={item.private} class:disabled={!item.enabled}>
            <span class="directory-icon"><Icon name={item.private ? 'lock' : 'folder'} size={19}/></span>
            <div class="directory-main"><strong>{item.name}</strong><div class="directory-badges">{#if item.private}<span class="badge private">私密</span>{/if}<span class="badge" class:neutral={!item.enabled}>{item.enabled ? '已启用' : '已停用'}</span>{#if !item.managed}<span class="badge neutral">主配置文件</span>{/if}</div><small class="muted">{item.code} · {location(item)}</small></div>
            {#if item.managed && canConfigure}<div class="button-group"><button class="button secondary small" disabled={busy} onclick={() => openEditor(item)}>编辑</button><button class="button secondary small danger-text" disabled={busy} onclick={() => removing = item}>移除</button></div>{:else}<small class="muted">{item.managed ? '管理员配置' : '在配置文件中修改'}</small>{/if}
          </div>
          {#if removing?.code === item.code}<div class="directory-confirm"><p>移除「{item.name}」的目录配置？原文件与已有索引保留。{#if item.private}<br/><strong>移除后这个目录不再受保险箱保护，会重新出现在文件页里。</strong>{/if}</p>{#if item.private && !vault.status?.unlocked}<VaultUnlock message="移除私密目录需要先解锁保险箱。"/>{/if}<div class="button-group"><button class="button secondary small" onclick={() => removing = null}>取消</button><button class="button primary small" disabled={pending || (item.private && !vault.status?.unlocked)} onclick={() => removeDirectory(item)}>确认移除目录配置</button></div></div>{/if}
        {/each}
        {#if !directories.length}<p class="section-empty">还没有照片目录。{canConfigure ? '点击“添加目录”选择一个存储位置里的文件夹。' : ''}</p>{/if}
      </div>
      {#if !canConfigure}<p class="field-help settings-card-foot">目录配置由管理员管理，下方可执行限量任务。</p>{/if}
    </section>
    <section class="panel settings-card">
      <header class="settings-card-head"><span class="settings-card-icon"><Icon name="jobs" size={19}/></span><div><h2>处理相册</h2><p class="muted">扫描会走完整个目录，自动分批建立索引；缩略图同样分批生成。每批之间检查内存和运行模式，中途停止后再次开始会从剩下的文件继续。</p></div></header>
      {#if !configuration.worker_available}<p class="notice warning">相册处理程序不可用。请检查 nasctl 可执行文件或 hub.album.worker_binary 配置。</p>{/if}
      <div class="settings-fields work-fields">
        <label class="field">处理目录<select aria-label="相册处理集合" bind:value={workCollection}><option value="" disabled>选择目录</option>{#each directories.filter((item) => item.enabled) as item}<option value={item.code}>{item.name}{item.private ? '（私密）' : ''}</option>{/each}</select></label>
        <label class="field">运行时间<select aria-label="相册任务时限" bind:value={workRuntime}>{#each runtimes as [value,label]}<option {value}>{label}</option>{/each}</select></label>
      </div>
      <div class="settings-card-actions"><button class="button primary" disabled={busy || !configuration.worker_available || !workCollection} onclick={() => {chained = true; startWork('scan');}}>扫描并生成缩略图</button><button class="button secondary" disabled={busy || !configuration.worker_available || !workCollection} onclick={() => startWork('scan')}>只扫描</button><button class="button secondary" disabled={busy || !configuration.worker_available || !workCollection} onclick={() => startWork('thumbs')}>只生成缩略图</button><span class="spacer"></span><button class="button secondary" disabled={busy || !configuration.worker_available} onclick={() => startWork(status?.paused ? 'resume' : 'pause')}>{status?.paused ? '恢复处理许可' : '暂停后续处理'}</button></div>
    </section>
  {/if}
{/if}
{#if readOnly}<p class="notice warning">控制台当前为只读模式，目录和任务操作会被拒绝。</p>{/if}
</div>
{#if editorOpen}<Dialog title={editing ? '编辑照片目录' : '添加照片目录'} description="原文件不会被移动、修改或删除。" onclose={closeEditor}>
  <form class="album-directory-form" onsubmit={saveDirectory}>
    <div class="settings-fields">
      <label class="field">名称<input aria-label="相册目录名称" bind:value={directoryName} required maxlength="128" placeholder="家庭照片"/></label>
      <label class="field">集合代码<input aria-label="相册集合代码" bind:value={code} required maxlength="32" pattern={'[a-z0-9][a-z0-9_\\-]{0,31}'} disabled={editing} placeholder="photos"/><small class="field-help">小写字母、数字、- 或 _，保存后不能修改</small></label>
      <label class="field">存储位置<select aria-label="相册存储位置" bind:value={directoryRoot} required><option value="" disabled>选择存储位置</option>{#each roots as root}<option value={root.id} disabled={!root.available}>{root.label || root.id}{!root.available ? '（不可用）' : ''}</option>{/each}</select></label>
      <label class="field">相对目录<input aria-label="相册相对目录" bind:value={directoryPath} maxlength="4096" placeholder="留空使用整个存储位置"/></label>
    </div>
    <div class="switch-list">
      <label class="switch-row"><span><strong>启用此集合</strong><small>停用后保留索引，但不再处理新文件。</small></span><input type="checkbox" role="switch" class="switch" bind:checked={enabled}/></label>
      <label class="switch-row" class:accent={privateDirectory}><span><strong>设为私密目录</strong><small>文件留在原处、不会被加密或移动；只有输入保险箱密码才能在相册和文件页里看到。不能放在普通相册目录里面，也不能包含普通相册目录。</small></span><input type="checkbox" role="switch" class="switch" bind:checked={privateDirectory}/></label>
      <label class="switch-row"><span><strong>允许照片与系统位于同一卷</strong><small>单盘设备需要打开；独立数据盘请保持关闭，以便发现挂载异常。</small></span><input type="checkbox" role="switch" class="switch" bind:checked={sharedVolume}/></label>
    </div>
    {#if needsUnlock}<VaultUnlock nested message={wasPrivate && !privateDirectory ? '取消私密需要先解锁保险箱。' : '设置或修改私密目录需要先解锁保险箱。'}/>{/if}
    {#if editorError}<ErrorNotice message={editorError}/>{/if}
    <div class="dialog-footer"><button type="button" class="button secondary" disabled={pending} onclick={closeEditor}>取消</button><button class="button primary" type="submit" disabled={pending || taskRunning || configuration?.busy || !directoryRoot || needsUnlock}>{pending ? '正在保存…' : editing ? '确认保存目录' : '确认添加目录'}</button></div>
  </form>
</Dialog>{/if}
