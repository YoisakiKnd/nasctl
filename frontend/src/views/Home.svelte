<script>
  import ErrorNotice from '../components/ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api, query } from '../lib/api.js';
  import { bytes, duration } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import SubscriptionCover from '../components/SubscriptionCover.svelte';
  import MediaPlayer from '../components/MediaPlayer.svelte';
  import DesktopApps from '../components/DesktopApps.svelte';
  import DesktopWidgets from '../components/DesktopWidgets.svelte';
  import { desktop, engines } from '../lib/desktop.svelte.js';
  let { auth, snapshot = null, error = '', adminPages = [], readOnly = true, canConfigure = false, onsearch } = $props();
  let watching = $state([]), subscriptions = $state([]), photos = $state([]), failed = $state({}), player = $state(null), q = $state('');
  let request, disposed = false;
  const roots = $derived(snapshot?.files?.roots || []);
  const searchRoot = $derived(roots.find((item) => item.available));
  const manager = $derived(canConfigure && auth?.principal !== 'ai');
  const cover = (episode) => subscriptions.find((sub) => sub.id === episode.subscription_id) || {title:episode.series, season:episode.season};

  // Each shelf is independent: a missing module hides its shelf, never the page.
  async function load() {
    request?.abort(); request = new AbortController(); const options = {signal:request.signal};
    const [anime, album] = await Promise.allSettled([api('/api/v1/anime/status',options),api('/api/v1/album/status',options)]);
    if (disposed) return;
    const jobs = [];
    if (anime.status === 'fulfilled' && anime.value.configured) jobs.push(Promise.all([api(query('/api/v1/anime/episodes',{state:'archived',watch:'in_progress',limit:6,offset:0}),options),api('/api/v1/anime/subscriptions',options)]).then(([episodes, subs]) => {if (!disposed) {watching = episodes.episodes || []; subscriptions = subs.subscriptions || [];}}));
    if (album.status === 'fulfilled' && album.value.connected && album.value.index_ready !== false) jobs.push(api(query('/api/v1/album/media',{limit:12,sort:'time_desc'}),options).then((page) => {if (!disposed) photos = page.items || [];}));
    await Promise.allSettled(jobs);
  }
  const engine = $derived(engines.find((item) => item.id === desktop.engine) || engines[0]);
  // Built-in apps first, as CasaOS lists its own apps; links follow.
  const apps = $derived([
    {href:'#files', label:'文件', icon:'folder', tone:'blue'},
    {href:'#album', label:'相册', icon:'album', tone:'violet'},
    {href:'#anime', label:'追番', icon:'anime', tone:'rose'},
    ...(adminPages.some((page) => page.id === 'admin/backup') ? [{href:'#admin/backup', label:'备份', icon:'backup', tone:'teal'}] : []),
    ...(adminPages.some((page) => page.id === 'admin/jobs') ? [{href:'#admin/jobs', label:'任务中心', icon:'jobs', tone:'amber'}] : []),
    {href:adminPages.length ? '#' + adminPages[0].id : '#me', label:'设置', icon:'settings', tone:'slate'},
  ]);
  function search(event) {
    event.preventDefault();
    const text = q.trim().slice(0, 128);
    if (!text) return;
    if (engine.url) {window.open(engine.url + encodeURIComponent(text), '_blank', 'noopener,noreferrer'); return;}
    if (searchRoot) onsearch?.({q:text, root:searchRoot.id});
  }
  onMount(() => {
    load();
    const back = () => {if (/^#\/?(home)?$/.test(window.location.hash)) load();};
    window.addEventListener('hashchange', back);
    return () => {disposed = true; request?.abort(); window.removeEventListener('hashchange', back);};
  });
</script>
<div class="desktop">
  <DesktopWidgets {roots}/>
  <div class="desktop-main">
    <form class="glass desktop-search" role="search" onsubmit={search}>
      <Icon name="search" size={19}/>
      <input aria-label="首页搜索文件" placeholder={engine.url ? '用' + engine.label + '搜索网页…' : searchRoot ? '在“' + (searchRoot.label || searchRoot.id) + '”中搜索文件…' : '暂无可搜索的存储目录'} bind:value={q} maxlength="128" disabled={!engine.url && !searchRoot}/>
      <select aria-label="搜索范围" bind:value={desktop.engine}>{#each engines as item (item.id)}<option value={item.id}>{item.label}</option>{/each}</select>
    </form>

    {#if manager}
      {#if auth.setup_requires_config}<div class="glass desktop-tip"><div><strong>启用账号登录</strong><p>先在本机配置持久登录保护，再为成员创建账号。</p></div><a class="button primary small" href="#admin/users">查看设置步骤</a></div>
      {:else if auth.setup_available && !auth.accounts_enabled}<div class="glass desktop-tip"><div><strong>设置管理员账号</strong><p>使用用户名和密码登录你的 NAS，并为成员分配角色。</p></div><a class="button primary small" href="#admin/users">开始设置</a></div>
      {:else if snapshot && roots.length === 0}<div class="glass desktop-tip"><div><strong>添加你的存储目录</strong><p>选择 NAS 上已准备好的目录，开始浏览和管理文件。</p></div><a class="button primary small" href="#admin/system">添加目录</a></div>{/if}
    {/if}
    {#if error && !snapshot}<ErrorNotice message={'设备状态读取失败：' + error}/>{/if}

    <section class="desktop-section" aria-labelledby="desktop-apps">
      <h2 id="desktop-apps">应用</h2>
      <div class="desktop-apps">
        {#each apps as app (app.href + app.label)}<a class={'app-tile builtin ' + app.tone} href={app.href}><span class="app-icon"><Icon name={app.icon} size={30}/></span><strong>{app.label}</strong></a>{/each}
        <DesktopApps canConfigure={manager && !readOnly}/>
      </div>
    </section>

    {#if watching.length}<section class="desktop-section" aria-labelledby="home-watching">
      <div class="section-title"><h2 id="home-watching">继续观看</h2><a class="text-button" href="#anime">全部追番<Icon name="chevron" size={14}/></a></div>
      <div class="shelf">{#each watching as episode (episode.id)}<button class="glass continue-card" onclick={() => player = episode}><SubscriptionCover subscription={cover(episode)}/><span><strong>{episode.series}</strong><small>第 {episode.season} 季 · 第 {episode.number} 集</small><progress value={episode.watch_position} max={episode.watch_duration || 1} aria-label="继续观看进度"></progress><small>{duration(episode.watch_position)} / {duration(episode.watch_duration)}</small></span><i><Icon name="play" size={20}/></i></button>{/each}</div>
    </section>{/if}

    {#if photos.length}<section class="desktop-section" aria-labelledby="home-photos">
      <div class="section-title"><h2 id="home-photos">最近照片</h2><a class="text-button" href="#album">打开相册<Icon name="chevron" size={14}/></a></div>
      <div class="photo-strip">{#each photos as item (item.id)}<a class="photo-thumb" href="#album" aria-label={'在相册中查看 ' + item.name} title={item.name}>{#if failed[item.id]}<Icon name={item.kind === 'video' ? 'play' : 'album'} size={28}/>{:else}<img src={item.thumb_url} alt="" loading="lazy" decoding="async" onerror={() => failed[item.id] = true}/>{/if}{#if item.kind === 'video'}<span class="album-video-mark"><Icon name="play" size={12}/></span>{/if}</a>{/each}</div>
    </section>{/if}
  </div>
</div>
{#if player}<MediaPlayer episode={player} {readOnly} onclose={() => {player = null; load();}}/>{/if}
