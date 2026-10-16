<script>
  import { getContext, onMount, tick } from 'svelte';
  const windowVisible = getContext('nasctl-window-visible') || (() => true);
  import { createAnimeController } from '../lib/anime.svelte.js';
  import { api } from '../lib/api.js';
  import { episodeLabels, time, duration } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import Dialog from '../components/Dialog.svelte';
  import Pager from '../components/Pager.svelte';
  import SubscriptionForm from '../components/SubscriptionForm.svelte';
  import AnimeSettings from '../components/AnimeSettings.svelte';
  import SubscriptionCover from '../components/SubscriptionCover.svelte';
  import MediaPlayer from '../components/MediaPlayer.svelte';
  import TaskMonitor from '../components/TaskMonitor.svelte';

  let { readOnly = false, viewer = false } = $props();
  const controller = createAnimeController({readSettings:()=>!viewer});
  const data = controller.data;
  let editor = $state(false);
  let editing = $state(null);
  let player = $state(null);
  let retry = $state(null);
  let downloadControl = $state(null);
  const controlLabels = {pause:'暂停下载',resume:'恢复下载',recheck:'重新校验'};
  let metadata = $state(null), metadataLimit = $state(20), metadataAfter = $state(0), metadataReplace = $state(false);
  let historyTarget = $state(null), historyStats = $state(null), historyError = $state(''), historyName = $state(''), historyLimit = $state(500), historyBefore = $state(''), historyDetach = $state(false), historyRemove = $state(false), historyLoading = $state(false);
  let historyRequest;
  function editMetadata(sub) {metadata = sub; metadataLimit = 20; metadataAfter = 0; metadataReplace = false;}
  async function editHistory(sub) {
    historyRequest?.abort(); historyRequest = new AbortController(); const current = historyRequest;
    historyTarget = sub; historyStats = null; historyError = ''; historyName = ''; historyLimit = 500; historyBefore = ''; historyDetach = false; historyRemove = false; historyLoading = true;
    try {const result = await api('/api/v1/anime/subscriptions/' + sub.id + '/history',{signal:current.signal}); if (historyRequest === current && !current.signal.aborted) historyStats = result;}
    catch (failure) {if (!current.signal.aborted) historyError = failure.message;} finally {if (historyRequest === current) historyLoading = false;}
  }
  function closeHistory() {if (data.pending) return; historyRequest?.abort(); historyTarget = null;}
  async function clearHistory() {
    const options = {confirm_title:historyName,limit:historyLimit,before:historyBefore ? Math.floor(new Date(historyBefore + 'T00:00:00').getTime()/1000) : 0,detach_downloads:historyDetach,remove_subscription:historyRemove};
    if (await controller.cleanupHistory(historyTarget.id,options)) closeHistory();
  }

  let tab = $state('subscriptions'), managing = $state(false);
  function toggleManaging() {managing = !managing; if (!managing) {subscriptionFilter='active'; if(tab==='settings')tab='subscriptions';}}
  function episodeCover(episode) {return data.subscriptions.find(sub => sub.id === episode.subscription_id) || {title:episode.series,season:episode.season};}
  let search = $state('');
  let subscriptionFilter = $state('active'), management = $state(null);
  const activeSubscriptions = $derived(data.subscriptions.filter((sub) => !sub.archived_at));
  const visible = $derived(data.subscriptions.filter((sub) => sub.title.toLowerCase().includes(search.toLowerCase()) && (subscriptionFilter === 'all' || (subscriptionFilter === 'archived' ? sub.archived_at : !sub.archived_at && (subscriptionFilter !== 'paused' || !sub.enabled)))));
  const counts = $derived(data.status?.counts || {});
  const busy = $derived(data.pending || data.status?.running || data.taskActive || readOnly);
  let dirty = $state(false);
  let discard = $state(false), keepButton = $state();
  function edit(subscription = null) { editing = subscription; dirty = false; discard = false; editor = true; }
  async function closeEditor() {
    if (data.pending) return;
    if (!dirty) { editor = false; return; }
    discard = true;
    await tick(); keepButton?.focus();
  }
  function filterEpisodes(value) { tab = 'episodes'; controller.filter(value); }
  function showSeries(id) { tab = 'episodes'; controller.selectSubscription(id); }
  async function save(subscription) { if (await controller.save(subscription)) editor = false; }
  function toggleSubscription(sub) { if (!sub.enabled && sub.auto_download) management = {operation:'enable',subscription:sub}; else controller.setEnabled(sub); }
  async function confirmManagement() {if (!management) return; const result = management.operation === 'archive' ? await controller.archive(management.subscription) : await controller.setEnabled(management.subscription); if (result) management = null;}
  function episodeSubscriptionArchived(episode) {return Boolean(data.subscriptions.find((sub) => sub.id === episode.subscription_id)?.archived_at);}
  onMount(() => {
    controller.load();
    const tick = () => { if (!document.hidden && windowVisible() && !data.pending) controller.load({ quiet: true }); };
    const interval = setInterval(tick, 15000);
    document.addEventListener('visibilitychange', tick);
    return () => { clearInterval(interval); document.removeEventListener('visibilitychange', tick); historyRequest?.abort(); controller.dispose(); };
  });
</script>
{#snippet episodeActions(episode)}{#if episode.state === 'archived'}<div class="button-group"><button class="button secondary small" onclick={() => player = episode}><Icon name="play" size={14}/>{episode.watch_position > 0 && !episode.watched ? '继续观看' : '播放'}</button><button class="text-button" disabled={data.pending || readOnly} onclick={() => controller.markWatched(episode)}>{episode.watched ? '标记未看' : '标记看完'}</button></div>
            {:else if episodeSubscriptionArchived(episode)}<span class="muted">订阅已归档</span>{:else}<div class="button-group"><button class="button secondary small" disabled={busy} onclick={() => ['discovered','ignored'].includes(episode.state) ? controller.queue(episode) : retry = episode}>{episode.state === 'discovered' ? '选择下载' : episode.state === 'ignored' ? '重新选择下载' : '重新核对'}</button>{#if episode.ever_submitted}<button class="text-button" disabled={busy} onclick={() => downloadControl = {episode,action:episode.state === 'paused' ? 'resume' : 'pause'}}>{episode.state === 'paused' ? '恢复下载' : '暂停下载'}</button><button class="text-button" disabled={busy} onclick={() => downloadControl = {episode,action:'recheck'}}>重新校验</button>{/if}{#if episode.state === 'discovered' || (episode.state === 'queued' && episode.attempts === 0 && !episode.ever_submitted && !episode.hash)}<button class="text-button" disabled={busy} onclick={() => controller.ignore(episode)}>忽略本集</button>{/if}</div>{/if}{/snippet}
<div class="anime-view" class:daily={!managing}>
<header class="page-heading anime-heading">
  <div><h1>{managing ? '管理追番' : '我的追番'}</h1><p class="muted">{managing ? '订阅、下载器和媒体库设置' : '今天，也有喜欢的故事。'}</p></div>
  <div class="button-group"><button class="icon-button" aria-label="刷新订阅" title="刷新订阅" disabled={!data.status?.configured || busy} onclick={() => controller.refresh()}><Icon name="refresh" size={19}/><span class="sr-only">刷新订阅</span></button>{#if !viewer}<button class="icon-button" aria-label={managing ? '返回追番' : '管理追番'} title={managing ? '返回追番' : '管理追番'} onclick={toggleManaging}><Icon name={managing ? 'back' : 'settings'} size={19}/></button>{/if}<button class="button primary" disabled={!data.status?.configured || data.pending || readOnly} onclick={() => edit()}><Icon name="plus" size={17}/>添加订阅</button></div>
</header>
{#if data.error}<div class="notice error" role="alert">{data.error}<button class="text-button" onclick={() => controller.load()}>重试</button></div>{/if}
{#if data.message}<div class="notice success" role="status">{data.message}</div>{/if}
{#if data.task}{#key data.task}<section class="panel task-panel"><TaskMonitor id={data.task} onupdate={(result) => controller.updateTask(result)} onsettled={() => controller.load({quiet:true})}/>{#if data.taskInfo?.kind === 'anime.metadata'}<p class="field-help">已处理 {data.taskInfo.result.episodes} 集，写入 {data.taskInfo.result.written} 个元数据文件，跳过 {data.taskInfo.result.skipped} 个。</p>{#if data.taskInfo.result.next_id && data.metadataOptions}<button class="button secondary small" disabled={busy} onclick={() => {const options = data.metadataOptions; controller.refreshMetadata(options.subscription,{limit:options.limit,replace:options.replace,after_id:data.taskInfo.result.next_id});}}>继续下一批元数据</button>{/if}{/if}{#if data.taskInfo?.kind === 'anime.history.cleanup'}<p class="field-help">已清理 {data.taskInfo.result.deleted} 条，剩余 {data.taskInfo.result.remaining} 条。{data.taskInfo.result.removed_subscription ? '订阅已移除。' : '订阅仍保留。'}</p>{#if data.historyOptions && data.taskInfo.result.deleted > 0 && data.taskInfo.result.remaining > 0 && !data.taskInfo.result.removed_subscription}<button class="button secondary small" disabled={busy} onclick={() => {const {subscription,...options} = data.historyOptions; controller.cleanupHistory(subscription,options);}}>继续清理下一批</button>{/if}{/if}<a class="text-button" href="#jobs">在任务中心查看</a></section>{/key}{/if}
{#if data.loading && !data.status}<div class="loading-state" role="status">正在读取追番数据…</div>
{:else if data.status && !data.status.configured}
  <section class="empty-state panel"><Icon name="anime" size={40}/><h2>开启你的第一季</h2><p>追番模块尚未启用。配置下载器和媒体库后，就可以添加 RSS 订阅。</p><a class="button secondary" href="#settings">前往设备设置</a></section>
{:else if data.status}
  {#if managing}<section class="stat-strip" aria-label="追番统计">
    <div><button class="stat-action" onclick={() => {tab = 'subscriptions'; subscriptionFilter = 'active';}}><span>订阅番剧</span><strong>{activeSubscriptions.length}<small>部</small></strong></button></div>
    <div><button class="stat-action" onclick={() => filterEpisodes('discovered')}><span>待选择</span><strong>{counts.discovered || 0}<small>集</small></strong></button></div>
    <div><button class="stat-action" onclick={() => filterEpisodes('downloading')}><span>下载中</span><strong>{counts.downloading || 0}<small>集</small></strong></button></div>
    <div><button class="stat-action" onclick={() => filterEpisodes('archived')}><span>已入库</span><strong>{counts.archived || 0}<small>集</small></strong></button></div>
  </section>{/if}
  {#if !managing && tab === 'subscriptions' && data.continueWatching?.length}<section class="continue-shelf"><h2>继续观看</h2><div>{#each data.continueWatching as episode(episode.id)}<button class="continue-card" onclick={() => player = episode}><SubscriptionCover subscription={episodeCover(episode)}/><span><strong>{episode.series}</strong><small>第 {episode.season} 季 · 第 {episode.number} 集</small><progress value={episode.watch_position} max={episode.watch_duration || 1} aria-label="继续观看进度"></progress><small>{duration(episode.watch_position)} / {duration(episode.watch_duration)}</small></span><i><Icon name="play" size={20}/></i></button>{/each}</div></section>{/if}
  <div class="section-bar">
    <nav class="segmented" aria-label="追番视图"><button class:active={tab === 'subscriptions'} onclick={() => tab = 'subscriptions'}>我的订阅 <span>{data.subscriptions.length}</span></button><button class:active={tab === 'episodes'} onclick={() => tab = 'episodes'}>剧集与媒体库</button>{#if !viewer && managing}<button class:active={tab === 'settings'} onclick={() => tab = 'settings'}>连接与调度</button>{/if}</nav>
    {#if tab === 'subscriptions'}<div class="button-group">{#if managing}<select aria-label="订阅筛选" bind:value={subscriptionFilter}><option value="active">当前订阅</option><option value="paused">已暂停</option><option value="archived">已归档</option><option value="all">全部订阅</option></select>{/if}<label class="search-field"><Icon name="search" size={17}/><input aria-label="搜索订阅" placeholder="搜索番剧…" bind:value={search}/></label></div>
    {:else if tab === 'episodes'}<div class="button-group"><select aria-label="番剧筛选" value={data.subscription} onchange={(event) => controller.selectSubscription(event.currentTarget.value)}><option value="0">全部番剧</option>{#each data.subscriptions as subscription}<option value={subscription.id}>{subscription.title} · 第 {subscription.season} 季</option>{/each}</select><select aria-label="剧集状态" value={data.filter} onchange={(event) => controller.filter(event.currentTarget.value)}><option value="">全部剧集</option>{#each Object.entries(episodeLabels) as [value, label]}<option {value}>{label}</option>{/each}</select><button class="button secondary small" disabled={busy} onclick={() => controller.sync(data.subscription || undefined)}>同步下载进度</button></div>{/if}
  </div>
  {#if tab === 'subscriptions'}
    {#if !data.subscriptions.length}<section class="empty-state panel"><Icon name="rss" size={38}/><h2>从一部喜欢的番剧开始</h2><p>添加 RSS 订阅，我们会为你发现新剧集、跟踪下载并整理入库。</p><button class="button primary" disabled={readOnly} onclick={() => edit()}><Icon name="plus" size={17}/>添加第一个订阅</button><small>默认手动选择下载，你可以随时开启自动下载。</small></section>
    {:else if !visible.length}<section class="empty-state panel"><Icon name="search" size={32}/><h2>没有匹配的订阅</h2><p>试试其他名称，或切换订阅筛选。</p></section>
    {:else}<div class="subscription-grid">{#each visible as sub (sub.id)}
      <article class="subscription-card panel">
        <button class="series-cover-button" aria-label={'查看 ' + sub.title + ' 的剧集'} onclick={() => showSeries(sub.id)}><SubscriptionCover subscription={sub}/></button>
        <div class="subscription-info"><div class="card-topline"><span class="season-label">第 {sub.season} 季</span><span class="badge" class:paused={!sub.enabled}>{sub.archived_at ? '已归档' : sub.enabled ? '追番中' : '已暂停'}</span></div><h2>{sub.title}</h2><p class="muted"><Icon name="rss" size={14}/>{sub.auto_download ? '自动下载' : '手动选择下载'}</p><p class="card-timestamp">{sub.last_checked ? time(sub.last_checked) + ' 刷新' : '尚未刷新'}</p>{#if sub.last_error}<p class="inline-error">{sub.last_error}</p>{/if}<div class="series-card-footer"><button class="text-button" onclick={() => showSeries(sub.id)}>查看剧集</button><details class="series-management" open={managing}><summary aria-label={'管理 ' + sub.title}><Icon name="more" size={19}/><span class="sr-only">更多</span></summary><div class="card-actions">{#if sub.archived_at}<button class="text-button" disabled={data.pending || readOnly} onclick={() => controller.archive(sub)}>恢复订阅</button><button class="text-button" disabled={busy} onclick={() => editHistory(sub)}>清理历史</button>{:else}<button class="text-button" disabled={busy} onclick={() => controller.refresh(sub.id)}><Icon name="refresh" size={14}/>刷新</button><button class="text-button" disabled={data.pending || readOnly} onclick={() => edit(sub)}><Icon name="edit" size={14}/>编辑</button><button class="text-button" disabled={data.pending || readOnly} onclick={() => toggleSubscription(sub)}>{sub.enabled ? '暂停' : '启用'}</button><button class="text-button" disabled={data.pending || readOnly} onclick={() => management = {operation:'archive',subscription:sub}}>归档</button>{/if}{#if !viewer}<a class="text-button" href={'/api/v1/anime/history/export?limit=20000&subscription=' + sub.id} download>导出记录</a>{/if}<button class="text-button" disabled={busy} onclick={() => editMetadata(sub)}>更新元数据</button></div></details></div></div>
      </article>
    {/each}</div>{/if}
  {:else if tab === 'settings'}
    <AnimeSettings settings={data.settings} connection={data.connection} pending={data.pending || data.status.running} {readOnly} oncheck={() => controller.checkConnection()} onlibrarychange={() => controller.load({quiet:true})}/>
  {:else}
    <div class="library-toolbar"><label>观看状态<select aria-label="观看状态" value={data.watch} onchange={(event) => controller.watch(event.currentTarget.value)}><option value="">不限</option><option value="in_progress">继续观看</option><option value="unwatched">未看完</option><option value="watched">已看完</option></select></label></div>
    <section class="panel episode-panel">
      {#if !data.episodes.length}<div class="empty-state"><Icon name="album" size={36}/><h2>这里还没有剧集</h2><p>刷新 RSS 后，匹配的单集会出现在这里。合集与无法识别集数的条目会跳过。</p></div>
      {:else if !managing}<div class="watch-episode-grid">{#each data.episodes as episode(episode.id)}<article class="watch-episode-card"><button class="episode-cover-button" aria-label={'播放 ' + episode.series + ' 第 ' + episode.number + ' 集'} disabled={episode.state !== 'archived'} onclick={() => player = episode}><SubscriptionCover subscription={episodeCover(episode)}/>{#if episode.state==='archived'}<span><Icon name="play" size={23}/></span>{/if}</button><div class="episode-name"><strong>{episode.series}</strong><span class="muted">第 {episode.season} 季 · 第 {episode.number} 集</span></div><span class="badge" class:complete={episode.state==='archived'} class:failed={episode.state==='failed'}>{episodeLabels[episode.state]||episode.state}</span>{#if episode.error}<p class="inline-error">{episode.error}</p>{/if}{#if episode.state==='archived'}<div class="daily-episode-actions">{@render episodeActions(episode)}</div>{:else}<div class="daily-episode-actions">{#if ['discovered','ignored'].includes(episode.state)}<button class="button secondary small" disabled={busy || episodeSubscriptionArchived(episode)} onclick={()=>controller.queue(episode)}>选择下载</button>{:else}<progress aria-label="下载进度" max="1" value={episode.progress||0}></progress>{/if}<details class="episode-more"><summary>更多操作</summary>{@render episodeActions(episode)}</details></div>{/if}</article>{/each}</div>
      {:else}<div class="table-wrap"><table><thead><tr><th>剧集</th><th>状态</th><th>进度</th><th><span class="sr-only">操作</span></th></tr></thead><tbody>
        {#each data.episodes as episode (episode.id)}<tr>
          <td class="episode-name"><strong>{episode.series}</strong><span class="muted">S{String(episode.season).padStart(2,'0')} · E{String(episode.number).padStart(2,'0')}</span><details><summary>发布信息</summary><p class="muted release-title">{episode.title}</p></details>{#if episode.error}<p class="inline-error">{episode.error}</p>{/if}</td>
          <td><span class="badge" class:complete={episode.state === 'archived'} class:failed={episode.state === 'failed'}>{episodeLabels[episode.state] || episode.state}</span></td>
          <td><div class="progress-cell">{#if episode.state === 'archived'}<progress max={episode.watch_duration || 1} value={episode.watched ? episode.watch_duration || 1 : episode.watch_position || 0} aria-label="观看进度"></progress><small>{episode.watched ? '已看完' : episode.watch_position > 0 ? duration(episode.watch_position) + ' / ' + duration(episode.watch_duration) : '未观看'}</small>{:else}<progress max="1" value={episode.progress || 0} aria-label="下载进度"></progress><small>{Math.round((episode.progress || 0) * 100)}%</small>{/if}</div></td>
          <td>{@render episodeActions(episode)}</td>
        </tr>{/each}
      </tbody></table></div>{/if}
      <Pager offset={data.offset} count={data.episodes.length} busy={data.loading || data.pending} onchange={(offset) => controller.page(offset)}/>
    </section>
  {/if}
  {#if managing}<footer class="page-note"><span class="status-dot" class:working={data.status.running}></span>{data.status.auto_poll ? '定时刷新已启用' : '手动刷新'}<span>·</span><span>{data.status.last_run ? '上次 RSS 刷新 ' + time(data.status.last_run) : '订阅添加后，点击刷新即可发现剧集'}</span>{#if data.status.last_sync}<span>·</span><span>下载同步 {time(data.status.last_sync)}</span>{/if}</footer>{/if}
  {#if readOnly}<p class="notice warning">控制台当前为只读模式，订阅修改和下载操作已禁用。</p>{/if}
  {#if data.status.last_error}<div class="notice error">上次刷新遇到问题：{data.status.last_error}</div>{/if}
{/if}
</div>
{#if editor}{#key editing?.id || 'new'}<Dialog title={editing ? '编辑订阅' : '添加订阅'} description="填写番剧信息，剩下的交给 nasctl。" onclose={closeEditor}>
  {#if discard}<section class="notice warning" aria-label="未保存的修改"><p>放弃尚未保存的订阅修改？</p><div class="button-group"><button bind:this={keepButton} class="button secondary small" onclick={() => discard = false}>继续编辑</button><button class="button secondary small" onclick={() => {dirty = false; editor = false;}}>放弃修改</button></div></section>{/if}
  <div inert={discard}><SubscriptionForm subscription={editing} pending={data.pending} metadataConfigured={Boolean(data.settings?.tmdb_configured)} onsave={save} oncancel={closeEditor} ondirty={() => dirty = true}/>{#if data.error}<p class="notice error" role="alert">{data.error}</p>{/if}</div>
</Dialog>{/key}{/if}
{#if retry}<Dialog title="重新核对下载" onclose={() => retry = null}><p>先检查下载器中是否已有这一集。未找到任务时，会重新提交下载。</p><div class="dialog-footer"><button class="button secondary" onclick={() => retry = null}>取消</button><button class="button primary" disabled={data.pending} onclick={async () => { if (await controller.queue(retry)) retry = null; }}>确认核对</button></div></Dialog>{/if}
{#if downloadControl}<Dialog title={controlLabels[downloadControl.action]} onclose={() => {if (!data.pending) downloadControl = null;}}><p>{downloadControl.episode.series} · 第 {downloadControl.episode.number} 集</p><p>{downloadControl.action === 'recheck' ? '下载器将重新读取文件校验完整性，期间可能增加磁盘负载。' : downloadControl.action === 'pause' ? '暂停这一集在下载器中的任务，已下载文件保留。' : '恢复这一集在下载器中的任务，继续下载。'}</p><p class="muted">只操作与这集标签和任务编号匹配的下载，不重新提交下载。</p>{#if data.error}<p class="notice error" role="alert">{data.error}</p>{/if}<div class="dialog-footer"><button class="button secondary" disabled={data.pending} onclick={() => downloadControl = null}>取消</button><button class="button primary" disabled={data.pending} onclick={async () => {if (await controller.controlDownload(downloadControl.episode,downloadControl.action)) downloadControl = null;}}>{data.pending ? '正在请求…' : '确认' + controlLabels[downloadControl.action]}</button></div></Dialog>{/if}
{#if player}<MediaPlayer episode={player} {readOnly} onclose={() => {player = null; controller.load({quiet:true});}}/>{/if}
{#if management}<Dialog title={management.operation === 'archive' ? '归档订阅' : '启用自动下载订阅'} onclose={() => {if (!data.pending) management = null;}}><p><strong>{management.subscription.title}</strong></p>{#if management.operation === 'archive'}<p>停止此订阅的发现、下载核对与归档，收进“已归档”。保留剧集、去重和观看记录，以及所有媒体文件。</p><p class="field-help">下载器中已提交的任务仍会继续运行。恢复订阅后保持暂停，需要另外启用。</p>{:else}<p>此订阅已开启自动下载。启用后将处理原有排队任务，并自动下载第 {management.subscription.start_episode} 集起{management.subscription.end_episode ? '至第 ' + management.subscription.end_episode + ' 集' : ''}的匹配历史剧集。</p><p class="field-help">如需手动选择下载，请先在编辑中关闭自动下载。</p>{/if}{#if data.error}<p class="notice error" role="alert">{data.error}</p>{/if}<div class="dialog-footer"><button class="button secondary" disabled={data.pending} onclick={() => management = null}>取消</button><button class="button primary" disabled={data.pending} onclick={confirmManagement}>{management.operation === 'archive' ? '确认归档订阅' : '确认启用订阅'}</button></div></Dialog>{/if}

{#if metadata}<Dialog title="更新已有媒体元数据" onclose={() => {if (!data.pending) metadata = null;}}><p>{metadata.title}</p><div class="album-directory-form"><label>每批最多处理集数<input aria-label="元数据处理限额" type="number" min="1" max="50" bind:value={metadataLimit}/></label><label>继续位置（首次为 0）<input aria-label="元数据继续位置" type="number" min="0" bind:value={metadataAfter}/></label><label class="check-row"><input type="checkbox" bind:checked={metadataReplace}/><span>替换已有 NFO 和封面</span></label></div><p class="muted">{metadataReplace ? '替换前会保存旧版本，可在“连接与调度”中恢复。' : '仅补齐缺失的 NFO 和封面，已有文件保留。'}每批最多 50 集、2 分钟，视频与字幕不改动。</p>{#if data.error}<p class="notice error" role="alert">{data.error}</p>{/if}<div class="dialog-footer"><button class="button secondary" disabled={data.pending} onclick={() => metadata = null}>取消</button><button class="button primary" disabled={busy || !metadataLimit || metadataLimit > 50 || metadataAfter < 0} onclick={async () => {if (await controller.refreshMetadata(metadata.id,{limit:metadataLimit,after_id:metadataAfter,replace:metadataReplace})) metadata = null;}}>{data.pending ? '正在创建任务…' : '确认更新元数据'}</button></div></Dialog>{/if}

{#if historyTarget}<Dialog title="清理已归档订阅历史" onclose={closeHistory}><p>{historyTarget.title}</p>{#if historyLoading}<p role="status">正在读取历史范围…</p>{/if}{#if historyError}<p class="notice error">{historyError}</p>{/if}{#if historyStats}<p class="muted">{historyStats.episodes} 条剧集记录，{historyStats.dedup} 条去重标记；{historyStats.linked} 条记录可能仍关联下载器。</p><a class="text-button" href={'/api/v1/anime/history/export?limit=20000&subscription=' + historyTarget.id} download>先导出这个订阅的记录</a><div class="album-directory-form"><label>每次最多清理<input aria-label="历史清理限额" type="number" min="1" max={historyRemove ? 20000 : 1000} bind:value={historyLimit}/></label><label>仅清理此日之前（可留空）<input aria-label="历史清理截止日期" type="date" bind:value={historyBefore} disabled={historyRemove}/></label><label class="check-row"><input type="checkbox" bind:checked={historyDetach}/><span>允许解除与已提交下载任务的关联</span></label><label class="check-row"><input type="checkbox" checked={historyRemove} onchange={(event) => {historyRemove = event.currentTarget.checked; if (historyRemove) {historyLimit = Math.max(1,historyStats.episodes); historyBefore = '';} else historyLimit = Math.min(historyLimit,1000);}}/><span>同时永久移除整个订阅</span></label></div><p>将永久移除选中历史和对应观看记录，请先导出。原视频和字幕保留。{historyDetach ? '下载器任务仍会继续，nasctl 不再跟踪这些记录。' : '默认保留仍可能关联下载器的记录。'}{!historyRemove ? '最小去重标记保留，防止重复下载。' : '整个订阅的去重标记也会移除。'}</p><label>输入完整订阅名称确认<input aria-label="确认历史清理订阅名称" bind:value={historyName} maxlength="200" autocomplete="off"/></label>{/if}{#if data.error}<p class="notice error" role="alert">{data.error}</p>{/if}<div class="dialog-footer"><button class="button secondary" disabled={data.pending} onclick={closeHistory}>取消</button><button class="button primary" disabled={busy || historyLoading || !historyStats?.archived || historyName !== historyTarget.title || !historyLimit || historyLimit > (historyRemove ? 20000 : 1000)} onclick={clearHistory}>{data.pending ? '正在创建任务…' : historyRemove ? '确认移除订阅及历史' : '确认清理历史'}</button></div></Dialog>{/if}
