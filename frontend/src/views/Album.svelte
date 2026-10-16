<script>
  import { onMount, tick } from 'svelte';
  import { api, query, json } from '../lib/api.js';
  import { duration } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import Dialog from '../components/Dialog.svelte';
  import PhotoViewer from '../components/PhotoViewer.svelte';
  import PhotoVault from '../components/PhotoVault.svelte';
  import TaskMonitor from '../components/TaskMonitor.svelte';
  let { readOnly = true, canConfigure = true, ondesktop } = $props();
  let managing = $state(false), configuration = $state(null), roots = $state([]), pending = $state(false), controlError = $state(''), controlMessage = $state('');
  let code = $state(''), directoryName = $state(''), directoryRoot = $state(''), directoryPath = $state(''), enabled = $state(true), sharedVolume = $state(false), editing = $state(false), removing = $state('');
  let workCollection = $state(''), workLimit = $state(50), workRuntime = $state('10m'), jobID = $state(''), taskRunning = $state(false);
  let status = $state(null), collections = $state([]), listing = $state(null), loading = $state(true), error = $state('');
  let collection = $state(''), search = $state(''), kind = $state(''), from = $state(''), to = $state(''), sort = $state('time_desc'), missing = $state(false), fallback = $state(false), gps = $state(false);
  let cursor = $state(''), history = $state([]), page = $state(1), failedThumbs = $state({});
  let applied = $state({});
  let view = $state('photos'), filtersOpen = $state(false), searchOpen = $state(false), menuOpen = $state(false), moveCandidate = $state(null), selected = $state(null);
  let searchInput = $state();
  let request, generation = 0, disposed = false;
  const groups = $derived.by(() => {
    const result = [];
    for (const item of listing?.items || []) {
      const month = /^\d{4}-\d{2}/.exec(item.taken_at || '')?.[0] || '';
      const key = applied.sort === 'name_asc' ? 'all' : month;
      let group = result.at(-1);
      if (!group || group.key !== key) {
        group = {key, label:key === 'all' ? '照片与视频' : month ? `${Number(month.slice(0,4))} 年 ${Number(month.slice(5,7))} 月` : '未记录日期', items:[]};
        result.push(group);
      }
      group.items.push(item);
    }
    return result;
  });
  function browse(nextView, nextCollection = '') {
    view = nextView; collection = nextCollection; kind = nextView === 'videos' ? 'video' : '';
    menuOpen = false; filtersOpen = false; searchOpen = false; search = ''; from = ''; to = ''; missing = false; fallback = false; gps = false;
    load({reset:true});
  }
  async function toggleSearch() {searchOpen = !searchOpen; menuOpen = false; if (searchOpen) {await tick(); searchInput?.focus();}}
  function dismissMenu(event) {if (event.key === 'Escape' && menuOpen) {event.preventDefault(); event.stopPropagation(); menuOpen = false;}}
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
  function open(item) {selected = item;}
  function close() {selected = null;}
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
  onMount(() => {load({reset:true}); return () => {disposed = true; request?.abort();};});
</script>
<svelte:window onkeydown={dismissMenu}/>
<div class="album-view">
  <header class="photo-toolbar">
    <div class="photo-title"><button class="icon-button photo-home" aria-label="返回 NAS 桌面" onclick={ondesktop}><Icon name="back" size={22}/></button><h1>{view === 'vault' ? '保险箱' : view === 'albums' ? '相簿' : collection ? collectionName(collection) : '照片'}</h1></div>
    <div class="photo-tools">
      {#if view !== 'vault' && view !== 'albums'}<button class="icon-button" class:chosen={searchOpen} aria-label="搜索照片" aria-expanded={searchOpen} onclick={toggleSearch}><Icon name="search" size={22}/></button>{/if}
      <button class="icon-button photo-menu-toggle" aria-label="相册更多操作" aria-expanded={menuOpen} onclick={() => menuOpen = !menuOpen}><Icon name="more" size={22}/></button>
    </div>
    {#if menuOpen}<div class="photo-menu" aria-label="相册操作">
      {#if status?.mode === 'local' && !readOnly}<button onclick={() => {menuOpen = false; manage();}}><Icon name="settings" size={18}/><span>管理相册</span></button>{/if}
      <button disabled={loading} onclick={() => {menuOpen = false; load();}}><Icon name="refresh" size={18}/><span>刷新相册</span></button>
      {#if view === 'photos' || view === 'videos'}<button aria-expanded={filtersOpen} onclick={() => {menuOpen = false; searchOpen = true; filtersOpen = !filtersOpen;}}><Icon name="sliders" size={18}/><span>筛选</span></button>{/if}
    </div>{/if}
  </header>
  {#if view === 'vault'}<PhotoVault {readOnly} candidate={moveCandidate} oncancel={() => moveCandidate = null} onmoved={() => {moveCandidate = null; load({reset:true});}}/>{:else}
  <div class="photo-library">
    {#if jobID}<TaskMonitor id={jobID} onsettled={() => {taskRunning = false; load({reset:true});}}/>{/if}
    {#if controlMessage}<p class="notice success" role="status">{controlMessage}</p>{/if}
    {#if error}<p class="notice error" role="alert">{error}<button class="text-button" onclick={() => load()}>重试</button></p>{/if}
    {#if loading && !status}<p class="loading-state" role="status">正在读取照片…</p>
    {:else if status && !status.configured}<section class="empty-state"><Icon name="album" size={42}/><h2>让回忆有个家</h2><p>启用本地相册后，就可以在这里浏览照片和视频。</p></section>
    {:else if status && !status.connected}<section class="empty-state"><Icon name="album" size={42}/><h2>暂时无法读取照片</h2><p>{status.error || '请检查照片文件夹是否可用。'}</p><button class="button secondary" disabled={loading} onclick={() => load()}>重试</button></section>
    {:else if status?.index_ready === false}<section class="empty-state"><Icon name="album" size={42}/><h2>你的第一本相册</h2><p>添加照片文件夹，把珍藏的照片和视频带到这里。</p>{#if !readOnly}<button class="button primary" onclick={manage}>{canConfigure ? '添加照片文件夹' : '查看相册任务'}</button>{:else}<p class="muted">请管理员添加照片文件夹。</p>{/if}</section>
    {:else if status?.connected}
      {#if status.paused}<p class="photo-status" title="已收藏的照片仍可浏览">相册处理已暂停</p>{/if}
      {#if view === 'albums'}
        <div class="photo-albums">{#each collections as album (album.code)}{@const cover = listing?.items.find((item) => item.collection === album.code && !failedThumbs[item.id])}<button class="photo-album" onclick={() => browse('photos',album.code)}><span class="photo-album-cover">{#if cover}<img src={cover.thumb_url} alt="" loading="lazy" decoding="async" onerror={() => failedThumbs[cover.id] = true}/>{:else}<Icon name="album" size={42}/>{/if}</span><strong>{album.name || album.code}</strong><span>{album.media_count} 项</span></button>{/each}</div>
        {#if !collections.length}<section class="empty-state"><Icon name="album" size={38}/><h2>还没有相簿</h2><p>添加的照片文件夹会显示在这里。</p>{#if !readOnly}<button class="button primary" onclick={manage}>添加照片文件夹</button>{/if}</section>{/if}
      {:else}
        <div class="photo-library-controls"><div class="photo-kind" aria-label="照片类型"><button class:chosen={!kind} onclick={() => {kind = ''; apply();}}>全部</button><button class:chosen={kind === 'image'} onclick={() => {kind = 'image'; apply();}}>照片</button><button class:chosen={kind === 'video'} onclick={() => {kind = 'video'; apply();}}>视频</button></div><span>{listing?.total_estimate >= 100000 ? '100,000+' : listing?.total_estimate || 0} 项</span></div>
        {#if searchOpen}<form class="album-filters" onsubmit={apply}>
          <div class="photo-search-row"><label class="photo-search"><Icon name="search" size={17}/><input type="search" aria-label="搜索相册" bind:this={searchInput} bind:value={search} maxlength="256" placeholder="搜索照片与视频"/></label><button class="icon-button" aria-label="提交照片搜索" title="搜索" disabled={loading} type="submit"><Icon name="forward" size={18}/></button><button class:chosen={filtersOpen} class="photo-filter-toggle" type="button" aria-expanded={filtersOpen} aria-controls="album-filter-options" onclick={() => filtersOpen = !filtersOpen}><Icon name="sliders" size={17}/>筛选</button></div>
          {#if filtersOpen}<div id="album-filter-options" class="photo-filter-options">
            <div class="album-filter-row"><label>相簿<select aria-label="相册集合" bind:value={collection}><option value="">全部相簿</option>{#each collections as item}<option value={item.code}>{item.name || item.code}</option>{/each}</select></label><label>类型<select aria-label="相册类型" bind:value={kind}><option value="">照片与视频</option><option value="image">照片</option><option value="video">视频</option><option value="other">其他</option><option value="unsupported">暂不支持</option></select></label><label>排序<select aria-label="相册排序" bind:value={sort}><option value="time_desc">最新在前</option><option value="time_asc">最早在前</option><option value="name_asc">名称</option></select></label><label>开始日期<input type="date" aria-label="相册开始日期" bind:value={from}/></label><label>结束日期<input type="date" aria-label="相册结束日期" bind:value={to}/></label></div>
            <div class="album-filter-row"><label class="check-row"><input type="checkbox" bind:checked={missing}/><span>显示缺失原文件</span></label><label class="check-row"><input type="checkbox" bind:checked={fallback}/><span>仅估计时间</span></label><label class="check-row"><input type="checkbox" bind:checked={gps}/><span>含定位信息</span></label><button class="button primary small" disabled={loading} type="submit">应用筛选</button></div>
          </div>{/if}
        </form>{/if}
        {#if applied.q || applied.from || applied.to || applied.include_missing || applied.gps || applied.fallback}<div class="photo-search-summary"><span>{applied.q ? '“' + applied.q + '”' : '已筛选'} · {listing?.total_estimate || 0} 项</span><button class="text-button" onclick={() => browse('photos',collection)}>清除筛选</button></div>{/if}
        {#if loading}<p class="loading-state" role="status">正在读取照片…</p>
        {:else if listing && !listing.items.length}<section class="empty-state"><Icon name="album" size={38}/><h2>这里还没有照片</h2><p>试试其他搜索条件，或将照片文件夹添加到相册。</p>{#if !readOnly}<button class="button secondary" onclick={manage}>管理照片文件夹</button>{/if}</section>
        {:else if listing}{#each groups as group}<section class="photo-month"><header><h2>{group.label}</h2></header><div class="album-gallery">{#each group.items as item (item.id)}<button class="album-tile" onclick={() => open(item)} aria-label={'查看 ' + item.name} title={item.name}><span class="album-frame">{#if failedThumbs[item.id]}<Icon name={item.kind === 'video' ? 'play' : 'album'} size={36}/>{:else}<img src={item.thumb_url} alt="" loading="lazy" decoding="async" onerror={() => failedThumbs[item.id] = true}/>{/if}{#if item.kind === 'video'}<span class="album-video-mark"><Icon name="play" size={13}/>{item.duration_ms ? duration(item.duration_ms/1000) : '视频'}</span>{/if}{#if item.missing}<span class="photo-missing" title="原文件暂不可用"><Icon name="warning" size={15}/></span>{/if}</span></button>{/each}</div></section>{/each}{/if}
        <footer class="photo-pager"><span>第 {page} 页</span><div class="button-group"><button class="text-button" disabled={loading || page === 1} onclick={() => load({reset:true})}>回到第一页</button><button class="icon-button" aria-label="上一页" title="上一页" disabled={loading || !history.length} onclick={previousPage}><Icon name="back" size={18}/></button><button class="icon-button" aria-label="下一页" title="下一页" disabled={loading || !listing?.next_cursor} onclick={nextPage}><Icon name="forward" size={18}/></button></div></footer>
      {/if}
    {/if}
  </div>
  {/if}
  <nav class="photo-tabs" aria-label="相册导航">
    <button class:chosen={view === 'photos' || view === 'videos'} aria-label="全部照片" aria-current={view === 'photos' || view === 'videos' ? 'page' : undefined} onclick={() => browse('photos')}><Icon name="photos" size={24}/><span>照片</span></button>
    <button class:chosen={view === 'albums'} aria-current={view === 'albums' ? 'page' : undefined} onclick={() => browse('albums')}><Icon name="albums" size={24}/><span>相簿</span></button>
    {#if canConfigure && status?.mode === 'local'}<button class:chosen={view === 'vault'} aria-current={view === 'vault' ? 'page' : undefined} onclick={() => {view = 'vault'; selected = null; menuOpen = false;}}><Icon name="lock" size={23}/><span>保险箱</span></button>{/if}
  </nav>
</div>
{#if managing}<Dialog title="管理本地相册" wide onclose={() => {if (!pending) managing = false;}}>
  {#if status?.paused}<p class="notice warning">相册处理已暂停{status.pause_reason ? '：' + status.pause_reason : ''}。已有照片仍可浏览。</p>{/if}
  {#each [...(status?.warnings || []),...(listing?.warnings || [])] as warning}<p class="notice warning">{warningText(warning)}</p>{/each}
  {#if listing?.degraded}<p class="notice warning">缩略图处理处于资源保护状态，仍可读取已有内容。</p>{/if}
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
{#if selected}<PhotoViewer {selected} items={listing?.items || []} onselect={open} onclose={close} {collectionName} onmove={!readOnly && canConfigure && status?.mode === 'local' ? (item) => {moveCandidate = item; close(); view = 'vault';} : undefined}/>{/if}
