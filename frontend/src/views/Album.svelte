<script>
  import { onMount } from 'svelte';
  import { api, query, json } from '../lib/api.js';
  import { bytes, duration } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import Dialog from '../components/Dialog.svelte';
  import TaskMonitor from '../components/TaskMonitor.svelte';
  let { readOnly = true, canConfigure = true } = $props();
  let managing = $state(false), configuration = $state(null), roots = $state([]), pending = $state(false), controlError = $state(''), controlMessage = $state('');
  let code = $state(''), directoryName = $state(''), directoryRoot = $state(''), directoryPath = $state(''), enabled = $state(true), sharedVolume = $state(false), editing = $state(false), removing = $state('');
  let workCollection = $state(''), workLimit = $state(50), workRuntime = $state('10m'), jobID = $state(''), taskRunning = $state(false);
  let status = $state(null), collections = $state([]), listing = $state(null), loading = $state(true), error = $state('');
  let collection = $state(''), search = $state(''), kind = $state(''), from = $state(''), to = $state(''), sort = $state('time_desc'), missing = $state(false), fallback = $state(false), gps = $state(false);
  let cursor = $state(''), history = $state([]), page = $state(1), failedThumbs = $state({});
  let applied = $state({});
  let selected = $state(null), detail = $state(null), detailError = $state(''), original = $state(false), imageError = $state('');
  let request, detailRequest, generation = 0, disposed = false;
  const timeLabels = {exif:'拍摄时间',container:'视频记录时间',filename:'文件名中的时间',filetime:'文件修改时间',none:'时间未记录'};
  function capture(value) {return value ? value.replace('T',' ').slice(0,19) : '时间未记录';}
  function mediaURL(item,part) {return '/api/v1/album/media/' + item.id + '/' + part;}
  function collectionName(code) {return collections.find((item) => item.code === code)?.name || code;}
  function warningText(value) {
    if (value.startsWith('no token is configured:')) return '尚未设置访问令牌，请仅在可信网络中使用。';
    if (status?.mode === 'local' && value.startsWith('a task is recorded as running.')) return '相册处理正在运行，已建立的索引仍可浏览。';
    return value;
  }
  async function load({ reset = false, next = cursor, nextHistory = history, nextPage = page } = {}) {
    const current = ++generation; request?.abort(); request = new AbortController(); loading = true; error = '';
    if (reset) {listing = null; next = ''; nextHistory = []; nextPage = 1;}
    const filters = reset || !listing ? {collection,q:search.trim(),kind,from,to,sort,include_missing:missing || undefined,fallback:fallback || undefined,gps:gps || undefined} : applied;
    const params = {...filters,cursor:next || undefined,limit:48};
    try {
      const state = await api('/api/v1/album/status',{signal:request.signal});
      if (disposed || current !== generation) return; status = state;
      if (!state.configured || !state.connected || state.index_ready === false) {listing = null; return;}
      const [groups,items] = await Promise.all([api('/api/v1/album/collections',{signal:request.signal}),api(query('/api/v1/album/media',params),{signal:request.signal})]);
      if (disposed || current !== generation) return;
      collections = groups.items || []; listing = items; cursor = next; history = nextHistory; page = nextPage; failedThumbs = {}; applied = filters;
    } catch (failure) {if (!disposed && current === generation && failure.name !== 'AbortError') error = failure.message;}
    finally {if (!disposed && current === generation) loading = false;}
  }
  function apply(event) {event?.preventDefault(); load({reset:true});}
  function nextPage() {if (!listing?.next_cursor) return; load({next:listing.next_cursor,nextHistory:[...history,cursor].slice(-100),nextPage:page+1});}
  function previousPage() {if (!history.length) return; load({next:history[history.length-1],nextHistory:history.slice(0,-1),nextPage:page-1});}
  async function open(item) {
    selected = item; detail = null; detailError = ''; original = false; imageError = ''; detailRequest?.abort(); detailRequest = new AbortController();
    try {const result = await api('/api/v1/album/media/' + item.id,{signal:detailRequest.signal}); if (!disposed && selected?.id === item.id) detail = result;}
    catch (failure) {if (!disposed && failure.name !== 'AbortError') detailError = failure.message;}
  }
  function close() {selected = null; detailRequest?.abort();}
  function resetDirectory() {code = ''; directoryName = ''; directoryPath = ''; enabled = true; sharedVolume = false; editing = false; removing = '';}
  function editDirectory(item) {code = item.code; directoryName = item.name; directoryRoot = item.root; directoryPath = item.path; enabled = item.enabled; sharedVolume = item.allow_shared_volume; editing = true; removing = '';}
  async function manage() {
    managing = true; pending = true; controlError = '';
    try {const [settings, locations] = await Promise.all([api('/api/v1/album/settings'),api('/api/v1/files/roots')]); if (disposed) return;
      configuration = settings; roots = locations.roots || []; directoryRoot ||= roots.find((item) => item.available)?.id || ''; workCollection ||= settings.directories.find((item) => item.enabled)?.code || '';
    } catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) pending = false;}
  }
  async function saveDirectory(event) {
    event.preventDefault(); if (pending||!canConfigure) return; pending = true; controlError = ''; controlMessage = '';
    try {const saved = await api('/api/v1/album/directories',json('POST',{code,name:directoryName,root:directoryRoot,path:directoryPath,enabled,allow_shared_volume:sharedVolume},{headers:{'X-Nasctl-Confirm':'1'}})); if (disposed) return;
      configuration = saved; workCollection ||= code; resetDirectory(); controlMessage = '目录已保存，已有照片不会被移动。可以开始一次限量扫描。'; await load({reset:true});
    } catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) pending = false;}
  }
  async function removeDirectory(value) {
    if (pending||!canConfigure) return; pending = true; controlError = '';
    try {const saved = await api('/api/v1/album/directories/' + encodeURIComponent(value),{method:'DELETE',headers:{'X-Nasctl-Confirm':'1'}}); if (disposed) return; configuration = saved; resetDirectory(); workCollection = saved.directories.find((item) => item.enabled)?.code || ''; controlMessage = '目录配置已移除，原文件与已有索引保留。'; await load({reset:true});}
    catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) pending = false;}
  }
  async function startWork(operation) {
    if (pending || taskRunning) return; pending = true; controlError = ''; controlMessage = '';
    const input = ['scan','thumbs'].includes(operation) ? {collection:workCollection,limit:workLimit,max_runtime:workRuntime} : operation === 'pause' ? {reason:'由相册操作者暂停'} : {};
    try {const accepted = await api('/api/v1/album/tasks/' + operation,json('POST',input,{headers:{'X-Nasctl-Confirm':'1'}})); if (disposed) return; jobID = accepted.job_id; taskRunning = true; managing = false;}
    catch (failure) {if (!disposed) controlError = failure.message;} finally {if (!disposed) pending = false;}
  }
  onMount(() => {load({reset:true}); return () => {disposed = true; request?.abort(); detailRequest?.abort();};});
</script>
<div class="album-view">
  <header class="page-heading"><div><p class="eyebrow">照片与视频</p><h1>相册</h1><p class="muted">把值得珍藏的瞬间，好好收藏。</p></div><div class="button-group">{#if status?.mode === 'local' && !readOnly}<button class="button secondary" onclick={manage}><Icon name="settings" size={16}/>管理相册</button>{/if}{#if status?.public_url}<a class="button secondary" href={status.public_url} target="_blank" rel="noopener noreferrer">打开相册服务<Icon name="external" size={16}/></a>{/if}<button class="button secondary" disabled={loading} onclick={() => load()}><Icon name="refresh" size={16}/>刷新</button></div></header>
  {#if jobID}<TaskMonitor id={jobID} onsettled={() => {taskRunning = false; load({reset:true});}}/>{/if}
  {#if controlMessage}<p class="notice success" role="status">{controlMessage}</p>{/if}
  {#if error}<p class="notice error" role="alert">{error}<button class="text-button" onclick={() => load()}>重试</button></p>{/if}
  {#if loading && !status}<p class="loading-state" role="status">正在连接相册…</p>
  {:else if status && !status.configured}<section class="empty-state panel"><Icon name="album" size={42}/><h2>相册已停用</h2><p>在配置中启用本地相册后，即可直接浏览照片与视频。</p><small>本地相册随 nasctl 运行，无需启动其他相册服务。</small></section>
  {:else if status && !status.connected}<section class="empty-state panel"><Icon name="album" size={42}/><h2>暂时无法读取相册</h2><p>{status.error || (status.mode === 'local' ? '本地索引暂不可用，请检查目录权限和索引状态。' : '请检查相册服务和连接配置。')}</p><button class="button secondary" disabled={loading} onclick={() => load()}>重试</button></section>
  {:else if status?.index_ready === false}<section class="empty-state panel"><Icon name="album" size={42}/><h2>还没有建立相册索引</h2><p>本地相册已启用。添加照片目录并建立索引后，照片和视频就会出现在这里。</p>{#if !readOnly}<button class="button primary" onclick={manage}>{canConfigure?'添加照片目录':'查看相册任务'}</button>{/if}<small>浏览只读取已有索引与缩略图，不会自动扫描或解码原文件。</small></section>
  {:else if status?.connected}
    {#if status.paused}<p class="notice warning">相册处理已暂停{status.pause_reason ? '：' + status.pause_reason : ''}。已建立的索引与缩略图仍可浏览。</p>{/if}
    {#each status.warnings || [] as warning}<p class="notice warning">{warningText(warning)}</p>{/each}
    <form class="panel album-filters" onsubmit={apply}>
      <div class="album-filter-row"><label>集合<select aria-label="相册集合" bind:value={collection}><option value="">全部集合</option>{#each collections as item}<option value={item.code}>{item.name || item.code} · {item.media_count} 项</option>{/each}</select></label><label class="album-search">搜索<input type="search" aria-label="搜索相册" bind:value={search} maxlength="256" placeholder="文件名或目录"/></label><label>类型<select aria-label="相册类型" bind:value={kind}><option value="">全部类型</option><option value="image">照片</option><option value="video">视频</option><option value="other">其他</option><option value="unsupported">暂不支持</option></select></label><label>排序<select aria-label="相册排序" bind:value={sort}><option value="time_desc">时间从新到旧</option><option value="time_asc">时间从旧到新</option><option value="name_asc">名称</option></select></label></div>
      <div class="album-filter-row"><label>开始日期<input type="date" aria-label="相册开始日期" bind:value={from}/></label><label>结束日期<input type="date" aria-label="相册结束日期" bind:value={to}/></label><label class="check-row"><input type="checkbox" bind:checked={missing}/><span>显示缺失原文件</span></label><label class="check-row"><input type="checkbox" bind:checked={fallback}/><span>仅估计时间</span></label><label class="check-row"><input type="checkbox" bind:checked={gps}/><span>含定位信息</span></label><button class="button primary" disabled={loading} type="submit">应用筛选</button></div>
    </form>
    {#each listing?.warnings || [] as warning}<p class="notice warning">{warning}</p>{/each}
    {#if listing?.degraded}<p class="notice warning">缩略图处理处于资源保护状态，仍可读取已有内容。</p>{/if}
    {#if loading}<p class="loading-state" role="status">正在读取相册…</p>
    {:else if listing && !listing.items.length}<section class="empty-state panel"><Icon name="album" size={38}/><h2>没有匹配的照片或视频</h2><p>换一个筛选条件，或在“管理相册”中建立一次限量索引。</p></section>
    {:else if listing}<div class="album-gallery">{#each listing.items as item (item.id)}<button class="album-tile panel" onclick={() => open(item)} aria-label={'查看 ' + item.name}><span class="album-frame">{#if failedThumbs[item.id]}<Icon name={item.kind === 'video' ? 'play' : 'album'} size={36}/>{:else}<img src={item.thumb_url} alt="" loading="lazy" decoding="async" onerror={() => failedThumbs[item.id] = true}/>{/if}{#if item.kind === 'video'}<span class="album-video-mark"><Icon name="play" size={16}/>{item.duration_ms ? duration(item.duration_ms/1000) : '视频'}</span>{/if}</span><span class="album-tile-info"><strong>{item.name}</strong><span>{capture(item.taken_at)}</span><small>{item.missing ? '原文件缺失' : item.taken_is_fallback ? '估计时间' : collectionName(item.collection)}</small></span></button>{/each}</div>{/if}
    <footer class="pager"><span>{listing?.total_estimate >= 100000 ? '至少 100,000' : listing?.total_estimate || 0} 项 · 第 {page} 页</span><div class="button-group"><button class="text-button" disabled={loading || page === 1} onclick={() => load({reset:true})}>回到第一页</button><button class="button secondary small" disabled={loading || !history.length} onclick={previousPage}>上一页</button><button class="button secondary small" disabled={loading || !listing?.next_cursor} onclick={nextPage}>下一页</button></div></footer>
    <p class="page-note">每页最多 48 项，保留最近 100 页的返回位置。缩略图缺失时显示占位图，不会在浏览时生成。</p>
  {/if}
</div>
{#if managing}<Dialog title="管理本地相册" wide onclose={() => {if (!pending) managing = false;}}>
  {#if controlError}<p class="notice error" role="alert">{controlError}</p>{/if}
  {#if pending && !configuration}<p role="status">正在读取目录设置…</p>{/if}
  {#if configuration}
    <section class="album-control-section"><h3>照片目录</h3><p class="muted">从已配置的存储位置添加目录。原文件保持原位，扫描只更新索引。</p>
      {#each configuration.directories as item}<div class="album-directory-row"><div><strong>{item.name}</strong><small class="muted">{item.code} · {item.root ? item.root + '/' : ''}{item.path || '根目录'} · {item.enabled ? '已启用' : '已停用'}</small></div>{#if item.managed&&canConfigure}<div class="button-group"><button class="text-button" disabled={pending || taskRunning || configuration.busy} onclick={() => editDirectory(item)}>编辑</button><button class="text-button" disabled={pending || taskRunning || configuration.busy} onclick={() => removing = item.code}>移除</button></div>{:else}<small class="muted">{item.managed?'管理员配置':'主配置文件'}</small>{/if}</div>{/each}
      {#if removing}<div class="notice"><p>移除 {removing} 的目录配置？原文件与已有索引保留。</p><div class="button-group"><button class="button secondary small" onclick={() => removing = ''}>取消</button><button class="button primary small" disabled={pending} onclick={() => removeDirectory(removing)}>确认移除目录配置</button></div></div>{/if}
      {#if canConfigure}<form class="album-directory-form" onsubmit={saveDirectory}>
        <label>集合代码<input aria-label="相册集合代码" bind:value={code} required maxlength="32" pattern={'[a-z0-9][a-z0-9_\\-]{0,31}'} disabled={editing} placeholder="photos"/></label><label>名称<input aria-label="相册目录名称" bind:value={directoryName} required maxlength="128" placeholder="家庭照片"/></label>
        <label>存储位置<select aria-label="相册存储位置" bind:value={directoryRoot} required><option value="" disabled>选择存储位置</option>{#each roots as root}<option value={root.id} disabled={!root.available}>{root.label || root.id}{!root.available ? '（不可用）' : ''}</option>{/each}</select></label><label>相对目录<input aria-label="相册相对目录" bind:value={directoryPath} maxlength="4096" placeholder="照片（留空使用根目录）"/></label>
        <label class="check-row"><input type="checkbox" bind:checked={enabled}/><span>启用此集合</span></label><label class="check-row"><input type="checkbox" bind:checked={sharedVolume}/><span>允许照片与系统位于同一卷</span></label>
        <p class="field-help">同卷选项适用于单盘设备；独立数据盘请保持关闭，以便检测挂载异常。</p><div class="button-group"><button class="button primary" type="submit" disabled={pending || taskRunning || configuration.busy || !directoryRoot}>{pending ? '正在保存…' : editing ? '确认保存目录' : '确认添加目录'}</button>{#if editing}<button type="button" class="button secondary" onclick={resetDirectory}>取消编辑</button>{/if}</div>
      </form>{:else}<p class="field-help">目录配置由管理员管理，下方可执行限量任务。</p>{/if}
    </section>
    <section class="album-control-section"><h3>限量处理</h3>{#if !configuration.worker_available}<p class="notice warning">相册处理组件尚未安装。将 albumctl 放在 nasctl 同目录，或配置 hub.album.worker_binary。它仅在执行任务时启动，无需单独运行服务。</p>{/if}
      <div class="album-directory-form"><label>处理集合<select aria-label="相册处理集合" bind:value={workCollection}><option value="" disabled>选择集合</option>{#each configuration.directories.filter((item) => item.enabled) as item}<option value={item.code}>{item.name}</option>{/each}</select></label><label>每次最多处理<input aria-label="相册任务限额" type="number" min="1" max="10000" bind:value={workLimit}/></label><label>最长运行时间<input aria-label="相册任务时限" bind:value={workRuntime} maxlength="16" placeholder="10m"/></label></div>
      <p class="field-help">扫描先建立索引，再按需生成缩略图。任务受运行模式、内存和挂载检查限制；进度与结果可在任务中心查看。</p><div class="button-group"><button class="button primary" disabled={pending || taskRunning || configuration.busy || !configuration.worker_available || !workCollection} onclick={() => startWork('scan')}>确认限量扫描</button><button class="button secondary" disabled={pending || taskRunning || configuration.busy || !configuration.worker_available || !workCollection} onclick={() => startWork('thumbs')}>确认生成缩略图</button><button class="button secondary" disabled={pending || taskRunning || configuration.busy || !configuration.worker_available} onclick={() => startWork(status?.paused ? 'resume' : 'pause')}>{status?.paused ? '恢复处理许可' : '暂停后续处理'}</button></div>
    </section>
  {/if}
  <div class="dialog-footer"><button class="button secondary" disabled={pending} onclick={() => managing = false}>关闭</button></div>
</Dialog>{/if}
{#if selected}<Dialog title={selected.name} wide onclose={close}>
  {#if detailError}<p class="notice error" role="alert">{detailError}</p>{:else if !detail}<p role="status">正在读取媒体详情…</p>{:else}<div class="album-detail">
    <div class="album-detail-preview">{#if detail.item.missing}<img src={mediaURL(detail.item,'thumb')} alt={detail.item.name}/><p class="notice warning">原文件暂不可用，显示已有缩略图。</p>
      {:else if detail.item.kind === 'video' && detail.item.playable}<video controls preload="metadata" src={mediaURL(detail.item,'raw')} onerror={() => imageError = '浏览器无法播放当前编码，可下载后使用本地播放器。'}><track kind="captions"/></video>
      {:else if detail.item.kind === 'image'}<img src={mediaURL(detail.item,original ? 'raw' : 'thumb')} alt={detail.item.name} onerror={() => imageError = '浏览器无法显示当前格式，可下载后使用本地应用打开。'}/>{#if !original}<button class="button secondary small" onclick={() => {original = true; imageError = '';}}>查看原图 · {bytes(detail.item.bytes)}</button>{/if}
      {:else}<img src={mediaURL(detail.item,'thumb')} alt=""/><p class="muted">这种格式需要下载后使用本地应用打开。</p>{/if}
      {#if imageError}<p class="notice error" role="alert">{imageError}</p>{/if}
    </div>
    <div><dl class="key-values"><dt>集合</dt><dd>{collectionName(detail.item.collection)}</dd><dt>位置</dt><dd>{detail.paths?.display || detail.item.name}</dd><dt>大小</dt><dd>{bytes(detail.item.bytes)}</dd><dt>{timeLabels[detail.item.taken_src] || '记录时间'}</dt><dd>{capture(detail.item.taken_at)}{detail.item.taken_is_fallback ? '（估计）' : ''}</dd>{#if detail.item.width && detail.item.height}<dt>尺寸</dt><dd>{detail.item.width} × {detail.item.height}</dd>{/if}{#if detail.item.make || detail.item.model}<dt>设备</dt><dd>{[detail.item.make,detail.item.model].filter(Boolean).join(' ')}</dd>{/if}{#if detail.item.duration_ms}<dt>时长</dt><dd>{duration(detail.item.duration_ms/1000)}</dd>{/if}{#if detail.item.gps}<dt>定位</dt><dd>{detail.item.gps.lat}, {detail.item.gps.lon}</dd>{/if}</dl>{#if Object.keys(detail.exif || {}).length}<details><summary>更多元数据</summary><dl class="key-values">{#each Object.entries(detail.exif) as [key,value]}<dt>{key}</dt><dd>{value}</dd>{/each}</dl></details>{/if}{#each detail.warnings || [] as warning}<p class="notice warning">{warning}</p>{/each}{#if detail.item.note}<p class="field-help">{detail.item.note}</p>{/if}</div>
  </div><div class="dialog-footer"><button class="button secondary" onclick={close}>关闭</button>{#if !detail.item.missing}<a class="button primary" href={mediaURL(detail.item,'download')} download={detail.item.name}><Icon name="download" size={16}/>下载原文件</a>{/if}</div>{/if}
</Dialog>{/if}
