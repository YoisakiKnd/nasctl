<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api, query } from '../lib/api.js';
  import { duration } from '../lib/format.js';
  import { vaultLockedResponse } from '../lib/vault.svelte.js';
  import Icon from './Icon.svelte';
  import PhotoViewer from './PhotoViewer.svelte';
  // Photos and videos from private album directories. They are read through
  // the /album/private/ routes, which answer only an unlocked vault session.
  let { canManage = false, readOnly = true, folders = 0 } = $props();
  let collections = $state([]), listing = $state(null), collection = $state(''), kind = $state(''), loading = $state(true), error = $state(''), selected = $state(null), failedThumbs = $state({});
  let cursor = $state(''), history = $state([]);
  let request, generation = 0, disposed = false;
  function collectionName(code) {return collections.find((item) => item.code === code)?.name || code;}
  async function load(next = '', nextHistory = []) {
    const mine = ++generation; request?.abort(); request = new AbortController(); loading = true; error = '';
    try {
      const [groups, items] = await Promise.all([
        api('/api/v1/album/private/collections', {signal:request.signal}),
        api(query('/api/v1/album/private/media', {collection, kind, cursor:next || undefined, limit:48}), {signal:request.signal}),
      ]);
      if (disposed || mine !== generation) return;
      collections = groups.items || []; listing = {...items, items:items.items.map((item) => ({...item, private:true}))}; cursor = next; history = nextHistory; failedThumbs = {};
    } catch (failure) {if (!disposed && mine === generation && failure.name !== 'AbortError' && !vaultLockedResponse(failure)) error = failure.message;}
    finally {if (!disposed && mine === generation) loading = false;}
  }
  function choose(code) {collection = code; load();}
  onMount(() => {load(); return () => {disposed = true; request?.abort();};});
</script>
<div class="private-library">
  {#if error}<ErrorNotice message={error} onretry={() => load(cursor, history)}/>{/if}
  {#if !folders}
    <section class="empty-state"><Icon name="folder" size={40}/><h2>还没有私密目录</h2><p>在“相册管理”里添加目录时打开“设为私密目录”。文件留在原来的位置，不会被移动或加密；不知道保险箱密码的人在相册和文件里都看不到它们。</p>{#if canManage && !readOnly}<a class="button primary" href="#admin/album">去设置私密目录</a>{/if}</section>
  {:else}
    <div class="private-filters">
      <div class="chip-row" aria-label="私密目录">
        <button class="chip" class:chosen={!collection} onclick={() => choose('')}>全部</button>
        {#each collections as item (item.code)}<button class="chip" class:chosen={collection === item.code} onclick={() => choose(item.code)}><Icon name="lock" size={13}/>{item.name || item.code}<small>{item.media_count}</small></button>{/each}
      </div>
      <div class="photo-kind" aria-label="私密内容类型"><button class:chosen={!kind} onclick={() => {kind = ''; load();}}>全部</button><button class:chosen={kind === 'image'} onclick={() => {kind = 'image'; load();}}>照片</button><button class:chosen={kind === 'video'} onclick={() => {kind = 'video'; load();}}>视频</button></div>
    </div>
    {#if loading && !listing}<p class="loading-state" role="status">正在读取私密内容…</p>
    {:else if listing && !listing.items.length}<section class="empty-state"><Icon name="album" size={38}/><h2>这里还没有内容</h2><p>私密目录需要先扫描。可以在“相册管理”的“限量处理”中选择私密目录并开始扫描。</p>{#if canManage && !readOnly}<a class="button secondary" href="#admin/album">去扫描</a>{/if}</section>
    {:else if listing}
      <div class="album-gallery private-gallery">{#each listing.items as item (item.id)}<button class="album-tile" onclick={() => selected = item} aria-label={'查看私密内容 ' + item.name} title={item.name}><span class="album-frame">{#if failedThumbs[item.id]}<Icon name={item.kind === 'video' ? 'play' : 'album'} size={34}/>{:else}<img src={item.thumb_url} alt="" loading="lazy" decoding="async" onerror={() => failedThumbs[item.id] = true}/>{/if}{#if item.kind === 'video'}<span class="album-video-mark"><Icon name="play" size={13}/>{item.duration_ms ? duration(item.duration_ms / 1000) : '视频'}</span>{/if}</span></button>{/each}</div>
      <footer class="photo-pager"><span>{listing.total_estimate || 0} 项</span><div class="button-group"><button class="icon-button" aria-label="私密内容上一页" disabled={loading || !history.length} onclick={() => load(history.at(-1), history.slice(0, -1))}><Icon name="back" size={18}/></button><button class="icon-button" aria-label="私密内容下一页" disabled={loading || !listing.next_cursor} onclick={() => load(listing.next_cursor, [...history, cursor].slice(-100))}><Icon name="forward" size={18}/></button></div></footer>
    {/if}
  {/if}
</div>
{#if selected}<PhotoViewer {selected} items={listing?.items || []} onselect={(item) => selected = item} onclose={() => selected = null} {collectionName}/>{/if}
