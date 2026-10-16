<script>
  import ErrorNotice from '../components/ErrorNotice.svelte';
  import { onMount, tick } from 'svelte';
  import { api, query } from '../lib/api.js';
  import { duration } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import PhotoViewer from '../components/PhotoViewer.svelte';
  import PhotoVault from '../components/PhotoVault.svelte';
  let { readOnly = true, canConfigure = true, canManage = false } = $props();
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
  onMount(() => {
    load({reset:true});
    // Coming back from the admin page refreshes the current page in place.
    const back = () => {if (/^#\/?album$/.test(window.location.hash) && !loading) load();};
    window.addEventListener('hashchange', back);
    return () => {disposed = true; request?.abort(); window.removeEventListener('hashchange', back);};
  });
</script>
<svelte:window onkeydown={dismissMenu}/>
<div class="album-view">
  <header class="photo-toolbar">
    <div class="photo-title"><h1>{view === 'vault' ? '保险箱' : view === 'albums' ? '相簿' : collection ? collectionName(collection) : '照片'}</h1></div>
    <div class="photo-tools">
      {#if view !== 'vault' && view !== 'albums'}<button class="icon-button" class:chosen={searchOpen} aria-label="搜索照片" aria-expanded={searchOpen} onclick={toggleSearch}><Icon name="search" size={22}/></button>{/if}
      <button class="icon-button photo-menu-toggle" aria-label="相册更多操作" aria-expanded={menuOpen} onclick={() => menuOpen = !menuOpen}><Icon name="more" size={22}/></button>
    </div>
    {#if menuOpen}<div class="photo-menu" aria-label="相册操作">
      {#if canManage}<a href="#admin/album" onclick={() => menuOpen = false}><Icon name="settings" size={18}/><span>管理相册</span></a>{/if}
      <button disabled={loading} onclick={() => {menuOpen = false; load();}}><Icon name="refresh" size={18}/><span>刷新相册</span></button>
      {#if view === 'photos' || view === 'videos'}<button aria-expanded={filtersOpen} onclick={() => {menuOpen = false; searchOpen = true; filtersOpen = !filtersOpen;}}><Icon name="sliders" size={18}/><span>筛选</span></button>{/if}
    </div>{/if}
  </header>
  {#if view === 'vault'}<PhotoVault {readOnly} candidate={moveCandidate} oncancel={() => moveCandidate = null} onmoved={() => {moveCandidate = null; load({reset:true});}}/>{:else}
  <div class="photo-library">
    {#if error}<ErrorNotice message={error} onretry={() => load()}/>{/if}
    {#if loading && !status}<p class="loading-state" role="status">正在读取照片…</p>
    {:else if status && !status.configured}<section class="empty-state"><Icon name="album" size={42}/><h2>让回忆有个家</h2><p>启用本地相册后，就可以在这里浏览照片和视频。</p></section>
    {:else if status && !status.connected}<section class="empty-state"><Icon name="album" size={42}/><h2>暂时无法读取照片</h2><p>{status.error || '请检查照片文件夹是否可用。'}</p><button class="button secondary" disabled={loading} onclick={() => load()}>重试</button></section>
    {:else if status?.index_ready === false}<section class="empty-state"><Icon name="album" size={42}/><h2>你的第一本相册</h2><p>添加照片文件夹，把珍藏的照片和视频带到这里。</p>{#if canManage && !readOnly}<a class="button primary" href="#admin/album">{canConfigure ? '添加照片文件夹' : '查看相册任务'}</a>{:else}<p class="muted">请管理员添加照片文件夹。</p>{/if}</section>
    {:else if status?.connected}
      {#if status.paused}<p class="photo-status" title="已收藏的照片仍可浏览">相册处理已暂停</p>{/if}
      {#if view === 'albums'}
        <div class="photo-albums">{#each collections as album (album.code)}{@const cover = listing?.items.find((item) => item.collection === album.code && !failedThumbs[item.id])}<button class="photo-album" onclick={() => browse('photos',album.code)}><span class="photo-album-cover">{#if cover}<img src={cover.thumb_url} alt="" loading="lazy" decoding="async" onerror={() => failedThumbs[cover.id] = true}/>{:else}<Icon name="album" size={42}/>{/if}</span><strong>{album.name || album.code}</strong><span>{album.media_count} 项</span></button>{/each}</div>
        {#if !collections.length}<section class="empty-state"><Icon name="album" size={38}/><h2>还没有相簿</h2><p>添加的照片文件夹会显示在这里。</p>{#if canManage && !readOnly}<a class="button primary" href="#admin/album">添加照片文件夹</a>{/if}</section>{/if}
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
        {:else if listing && !listing.items.length}<section class="empty-state"><Icon name="album" size={38}/><h2>这里还没有照片</h2><p>试试其他搜索条件，或将照片文件夹添加到相册。</p>{#if canManage && !readOnly}<a class="button secondary" href="#admin/album">管理照片文件夹</a>{/if}</section>
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

{#if selected}<PhotoViewer {selected} items={listing?.items || []} onselect={open} onclose={close} {collectionName} onmove={!readOnly && canConfigure && status?.mode === 'local' ? (item) => {moveCandidate = item; close(); view = 'vault';} : undefined}/>{/if}
