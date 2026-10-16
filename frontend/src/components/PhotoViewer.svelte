<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { onMount, untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { vaultLockedResponse } from '../lib/vault.svelte.js';
  import { bytes, duration } from '../lib/format.js';
  import Icon from './Icon.svelte';
  import Dialog from './Dialog.svelte';
  let { selected, items, onselect, onclose, onmove, collectionName = (code) => code } = $props();
  let detail = $state(null), detailError = $state(''), original = $state(true), imageError = $state(''), showInfo = $state(false), chromeHidden = $state(false);
  let touchStart, swiped = false;
  let request, generation = 0, disposed = false;
  const selectedIndex = $derived(items.findIndex((item) => item.id === selected.id));
  const timeLabels = {exif:'拍摄时间',container:'视频记录时间',filename:'文件名中的时间',filetime:'文件修改时间',none:'时间未记录'};
  function capture(value) {return value ? value.replace('T',' ').slice(0,19) : '时间未记录';}
  // Encrypted vault photos and private-directory media have their own gated routes.
  const scope = (item) => item.vault ? 'vault/' : item.private ? 'private/' : '';
  function mediaURL(item,part) {return '/api/v1/album/' + scope(selected) + 'media/' + item.id + '/' + part;}
  function navigate(offset) {const item = items[selectedIndex + offset]; if (item) onselect(item);}
  function touchDown(event) {touchStart = {id:event.pointerId,x:event.clientX,y:event.clientY}; swiped = false; event.currentTarget.setPointerCapture?.(event.pointerId);}
  function touchUp(event) {
    if (!touchStart || touchStart.id !== event.pointerId) return;
    const dx = event.clientX-touchStart.x, dy = event.clientY-touchStart.y; touchStart = null;
    if (Math.abs(dx) > 50 && Math.abs(dx) > Math.abs(dy)*1.5) {swiped = true; navigate(dx < 0 ? 1 : -1);}
  }
  function toggleChrome() {if (swiped) {swiped = false; return;} chromeHidden = !chromeHidden; if (chromeHidden) showInfo = false;}
  function photoKeys(event) {
    if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey || event.target.closest('input,textarea,select,video')) return;
    if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {event.preventDefault(); event.stopPropagation(); navigate(event.key === 'ArrowLeft' ? -1 : 1);}
  }
  async function load(item) {
    const current = ++generation; request?.abort(); request = new AbortController(); detail = null; detailError = ''; original = true; imageError = ''; showInfo = false;
    try {const result = await api('/api/v1/album/' + scope(item) + 'media/' + item.id,{signal:request.signal}); if (!disposed && generation === current) detail = result;}
    catch (error) {if (disposed || generation !== current || error.name === 'AbortError') return; if (!vaultLockedResponse(error)) detailError = error.message;}
  }
  $effect(() => {const item = selected; untrack(() => load(item));});
  onMount(() => () => {disposed = true; request?.abort();});
</script>
<Dialog title={selected.name} photo {chromeHidden} onclose={onclose} onkey={photoKeys}>
  <div class="photo-viewer-date" inert={chromeHidden}><strong>{capture(selected.taken_at).slice(0,10)}</strong><span>{selected.vault ? '保险箱' : (selected.private ? '私密 · ' : '') + collectionName(selected.collection)}</span></div>
  <div class="photo-lightbox-tools" inert={chromeHidden}><span>{selectedIndex + 1} / {items.length}</span><div>{#if onmove && !selected.vault && !selected.private && detail?.item.kind === 'image' && !detail.item.missing}<button class="icon-button" aria-label="移入保险箱" title="移入保险箱" onclick={() => onmove(selected)}><Icon name="lock" size={19}/></button>{/if}<button class="icon-button" class:chosen={showInfo} aria-label="照片信息" aria-expanded={showInfo} onclick={() => showInfo = !showInfo}><Icon name="info" size={20}/></button>{#if detail && !detail.item.missing}<a class="icon-button" aria-label="下载原文件" title="下载原文件" href={mediaURL(detail.item,'download')} download={detail.item.name}><Icon name="download" size={20}/></a>{/if}</div></div>
  {#if detailError}<ErrorNotice message={detailError}/>{:else if !detail}<p class="loading-state" role="status">正在打开照片…</p>{:else}
    <div class="album-detail" class:with-info={showInfo}>
      <div class="photo-stage">
        <button inert={chromeHidden} class="photo-step previous" aria-label="上一张" disabled={selectedIndex <= 0} onclick={() => navigate(-1)}><Icon name="back" size={24}/></button>
        <div class="album-detail-preview">
          {#key detail.item.id}
            {#if detail.item.missing}<img draggable="false" src={mediaURL(detail.item,'thumb')} alt={detail.item.name}/><p class="photo-caption">原文件暂不可用，显示已有缩略图。</p>
            {:else if detail.item.kind === 'video' && detail.item.playable}<video controls preload="metadata" src={mediaURL(detail.item,'raw')} onerror={() => imageError = '浏览器无法播放当前编码，可下载后使用本地播放器。'}><track kind="captions"/></video>
            {:else if detail.item.kind === 'image'}<button class="photo-image-toggle" aria-label="显示或隐藏浏览控件，左右滑动切换照片" onclick={toggleChrome} onpointerdown={touchDown} onpointerup={touchUp} onpointercancel={() => touchStart = null}><img draggable="false" src={mediaURL(detail.item,original ? 'raw' : 'thumb')} alt={detail.item.name} onerror={() => {if (original) {original = false; imageError = '原图暂时无法显示，已切换为缩略图。可下载原文件查看。';} else imageError = '照片暂时无法显示，请检查文件是否可用。';}}/></button>
            {:else}<img draggable="false" src={mediaURL(detail.item,'thumb')} alt=""/><p class="photo-caption">这种格式需要下载后使用本地应用打开。</p>{/if}
          {/key}
          {#if imageError}<p class="photo-caption" role="alert">{imageError}</p>{/if}
        </div>
        <button inert={chromeHidden} class="photo-step next" aria-label="下一张" disabled={selectedIndex < 0 || selectedIndex >= (items.length)-1} onclick={() => navigate(1)}><Icon name="forward" size={24}/></button>
      </div>
      {#if showInfo}<aside class="photo-info" aria-label="照片详情"><h3>照片信息</h3><dl class="key-values"><dt>集合</dt><dd>{selected.vault ? '照片保险箱' : collectionName(detail.item.collection)}</dd><dt>位置</dt><dd>{detail.paths?.display || detail.item.name}</dd><dt>大小</dt><dd>{bytes(detail.item.bytes)}</dd><dt>{timeLabels[detail.item.taken_src] || '记录时间'}</dt><dd>{capture(detail.item.taken_at)}{detail.item.taken_is_fallback ? '（估计）' : ''}</dd>{#if detail.item.width && detail.item.height}<dt>尺寸</dt><dd>{detail.item.width} × {detail.item.height}</dd>{/if}{#if detail.item.make || detail.item.model}<dt>设备</dt><dd>{[detail.item.make,detail.item.model].filter(Boolean).join(' ')}</dd>{/if}{#if detail.item.duration_ms}<dt>时长</dt><dd>{duration(detail.item.duration_ms/1000)}</dd>{/if}{#if detail.item.gps}<dt>定位</dt><dd>{detail.item.gps.lat}, {detail.item.gps.lon}</dd>{/if}</dl>{#if Object.keys(detail.exif || {}).length}<details><summary>更多元数据</summary><dl class="key-values">{#each Object.entries(detail.exif) as [key,value]}<dt>{key}</dt><dd>{value}</dd>{/each}</dl></details>{/if}{#each detail.warnings || [] as warning}<p class="notice warning">{warning}</p>{/each}{#if detail.item.note}<p class="field-help">{detail.item.note}</p>{/if}</aside>{/if}
    </div>

  {/if}
</Dialog>