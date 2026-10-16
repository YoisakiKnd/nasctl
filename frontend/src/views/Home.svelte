<script>
  import { onMount } from 'svelte';
  import { api, query } from '../lib/api.js';
  import { bytes, duration } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import SubscriptionCover from '../components/SubscriptionCover.svelte';
  import MediaPlayer from '../components/MediaPlayer.svelte';
  import DesktopApps from '../components/DesktopApps.svelte';
  let { auth, snapshot = null, error = '', adminPages = [], readOnly = true, canConfigure = false, onsearch } = $props();
  let watching = $state([]), subscriptions = $state([]), photos = $state([]), failed = $state({}), player = $state(null), q = $state(''), now = $state(new Date());
  let request, disposed = false;
  const roots = $derived(snapshot?.files?.roots || []);
  const searchRoot = $derived(roots.find((item) => item.available));
  const name = $derived(auth?.display_name || auth?.name || '');
  const greeting = $derived(now.getHours() < 6 ? '夜深了' : now.getHours() < 11 ? '早上好' : now.getHours() < 14 ? '中午好' : now.getHours() < 19 ? '下午好' : '晚上好');
  const manager = $derived(canConfigure && auth?.principal !== 'ai');
  const cover = (episode) => subscriptions.find((sub) => sub.id === episode.subscription_id) || {title:episode.series, season:episode.season};

  // Each shelf is independent: a missing module hides its shelf, never the page.
  async function load() {
    request?.abort(); request = new AbortController(); const options = {signal:request.signal}; now = new Date();
    const [anime, album] = await Promise.allSettled([api('/api/v1/anime/status',options),api('/api/v1/album/status',options)]);
    if (disposed) return;
    const jobs = [];
    if (anime.status === 'fulfilled' && anime.value.configured) jobs.push(Promise.all([api(query('/api/v1/anime/episodes',{state:'archived',watch:'in_progress',limit:6,offset:0}),options),api('/api/v1/anime/subscriptions',options)]).then(([episodes, subs]) => {if (!disposed) {watching = episodes.episodes || []; subscriptions = subs.subscriptions || [];}}));
    if (album.status === 'fulfilled' && album.value.connected && album.value.index_ready !== false) jobs.push(api(query('/api/v1/album/media',{limit:12,sort:'time_desc'}),options).then((page) => {if (!disposed) photos = page.items || [];}));
    await Promise.allSettled(jobs);
  }
  function search(event) {
    event.preventDefault();
    if (!q.trim() || !searchRoot) return;
    onsearch?.({q:q.trim().slice(0,128), root:searchRoot.id});
  }
  onMount(() => {
    load();
    const back = () => {if (/^#\/?(home)?$/.test(window.location.hash)) load();};
    window.addEventListener('hashchange', back);
    return () => {disposed = true; request?.abort(); window.removeEventListener('hashchange', back);};
  });
</script>
<div class="home">
  <header class="home-hero">
    <div><p class="eyebrow">{now.toLocaleDateString('zh-CN',{month:'long',day:'numeric',weekday:'long'})}</p><h1>{greeting}{name ? '，' + name : ''}</h1></div>
    <form class="home-search" role="search" onsubmit={search}><Icon name="search" size={18}/><input aria-label="首页搜索文件" placeholder={searchRoot ? '在“' + (searchRoot.label || searchRoot.id) + '”中搜索文件…' : '暂无可搜索的存储目录'} bind:value={q} maxlength="128" disabled={!searchRoot}/><button class="button primary small" type="submit" disabled={!q.trim() || !searchRoot}>搜索</button></form>
  </header>

  {#if manager}
    {#if auth.setup_requires_config}<div class="notice home-onboarding"><div><strong>启用账号登录</strong><p>先在本机配置持久登录保护，再为成员创建账号。</p></div><a class="button primary small" href="#admin/users">查看设置步骤</a></div>
    {:else if auth.setup_available && !auth.accounts_enabled}<div class="notice home-onboarding"><div><strong>设置管理员账号</strong><p>使用用户名和密码登录你的 NAS，并为成员分配角色。</p></div><a class="button primary small" href="#admin/users">开始设置</a></div>
    {:else if snapshot && roots.length === 0}<div class="notice home-onboarding"><div><strong>添加你的存储目录</strong><p>选择 NAS 上已准备好的目录，开始浏览和管理文件。</p></div><a class="button primary small" href="#admin/system">添加目录</a></div>{/if}
  {/if}

  {#if watching.length}<section class="home-section" aria-labelledby="home-watching">
    <div class="section-title"><h2 id="home-watching">继续观看</h2><a class="text-button" href="#anime">全部追番<Icon name="chevron" size={14}/></a></div>
    <div class="shelf">{#each watching as episode (episode.id)}<button class="continue-card" onclick={() => player = episode}><SubscriptionCover subscription={cover(episode)}/><span><strong>{episode.series}</strong><small>第 {episode.season} 季 · 第 {episode.number} 集</small><progress value={episode.watch_position} max={episode.watch_duration || 1} aria-label="继续观看进度"></progress><small>{duration(episode.watch_position)} / {duration(episode.watch_duration)}</small></span><i><Icon name="play" size={20}/></i></button>{/each}</div>
  </section>{/if}

  {#if photos.length}<section class="home-section" aria-labelledby="home-photos">
    <div class="section-title"><h2 id="home-photos">最近照片</h2><a class="text-button" href="#album">打开相册<Icon name="chevron" size={14}/></a></div>
    <div class="photo-strip">{#each photos as item (item.id)}<a class="photo-thumb" href="#album" aria-label={'在相册中查看 ' + item.name} title={item.name}>{#if failed[item.id]}<Icon name={item.kind === 'video' ? 'play' : 'album'} size={28}/>{:else}<img src={item.thumb_url} alt="" loading="lazy" decoding="async" onerror={() => failed[item.id] = true}/>{/if}{#if item.kind === 'video'}<span class="album-video-mark"><Icon name="play" size={12}/></span>{/if}</a>{/each}</div>
  </section>{/if}

  <div class="home-grid">
    <section class="panel home-storage" aria-labelledby="home-storage">
      <div class="panel-heading"><h2 id="home-storage">存储空间</h2><a class="text-button" href="#files">浏览文件<Icon name="chevron" size={14}/></a></div>
      {#if error && !snapshot}<p class="notice error" role="alert">设备状态读取失败：{error}</p>
      {:else if !snapshot}<p class="section-empty" role="status">正在读取…</p>
      {:else if !roots.length}<p class="section-empty">还没有可用的存储目录。</p>
      {:else}<ul class="storage-meter-list">{#each roots as root (root.id)}<li><a href="#files"><span class="storage-icon" class:offline={!root.available}><Icon name="disk" size={20}/></span><div><strong>{root.label || root.id}</strong><small>{!root.available ? '暂不可用' : root.capacity ? bytes(root.capacity.available_bytes) + ' 可用 · 共 ' + bytes(root.capacity.total_bytes) : '容量暂不可用'}</small>{#if root.capacity}<progress class="capacity-meter" value={root.capacity.used_bytes} max={root.capacity.total_bytes} aria-label={(root.label || root.id) + ' 文件系统空间使用率'}></progress>{/if}</div></a></li>{/each}</ul>{/if}
    </section>
    <section class="panel home-apps" aria-labelledby="home-apps">
      <div class="panel-heading"><h2 id="home-apps">我的应用</h2>{#if adminPages.length}<a class="text-button" href={'#' + adminPages[0].id}>管理后台<Icon name="chevron" size={14}/></a>{/if}</div>
      <div class="app-grid"><DesktopApps canConfigure={manager && !readOnly}/></div>
    </section>
  </div>
</div>
{#if player}<MediaPlayer episode={player} {readOnly} onclose={() => {player = null; load();}}/>{/if}
