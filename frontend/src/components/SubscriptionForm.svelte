<script>
  import { untrack, onDestroy } from 'svelte';
  import { api, json } from '../lib/api.js';
  import MetadataPicker from './MetadataPicker.svelte';
  import AnimeSourcePicker from './AnimeSourcePicker.svelte';
  let { subscription = null, pending = false, metadataConfigured = false, onsave, oncancel, ondirty } = $props();
  let metadataBusy = $state(false), sourceBusy=$state(false), sourceMessage=$state('');
  // The parent keys each dialog by subscription. Take a draft once so polls
  // cannot replace unsaved edits while the form is open.
  const initial = untrack(() => subscription);
  let title = $state(initial?.title || '');
  let feed = $state(initial?.feed_url || '');
  let include = $state(initial?.include || '');
  let exclude = $state(initial?.exclude || '');
  let season = $state(initial?.season || 1);
  let tmdb = $state(initial?.tmdb_id || 0);
  let start = $state(initial?.start_episode || 1);
  let end = $state(initial?.end_episode || 0);
  let preview = $state(null), previewing = $state(false), previewError = $state('');
  let previewRequest, generation = 0;
  onDestroy(() => { ++generation; previewRequest?.abort(); });
  let enabled = $state(initial?.enabled ?? true);
  let auto = $state(initial?.auto_download ?? false);
  let advanced = $state(Boolean(initial?.include || initial?.exclude || initial?.tmdb_id));

  function draft() {
    return { id: subscription?.id || 0, title: title.trim(), feed_url: feed.trim(), include: include.trim(), exclude: exclude.trim(), season, tmdb_id: tmdb || 0, enabled, auto_download: auto, start_episode: start || 1, end_episode: end || 0 };
  }
  function submit(event) { event.preventDefault(); onsave(draft()); }
  function changed() { ondirty?.(); preview = null; previewError = ''; ++generation; previewRequest?.abort(); previewing = false; }
  async function previewRules(event) {
    if (!event.currentTarget.form.reportValidity()) return;
    const current = ++generation;
    previewRequest?.abort(); previewRequest = new AbortController(); previewing = true; previewError = ''; preview = null;
    try { const result = await api('/api/v1/anime/preview', json('POST', draft(), { signal: previewRequest.signal })); if (current === generation) preview = result; }
    catch (error) { if (current === generation && error.name !== 'AbortError') previewError = error.message; }
    finally { if (current === generation) previewing = false; }
  }
</script>
<form class="subscription-form" onsubmit={submit} oninput={changed}>
  {#if !subscription}<AnimeSourcePicker disabled={pending} onbusy={value=>sourceBusy=value} onselect={value=>{title=value.title;feed=value.feed_url;include=value.include;sourceMessage='订阅源已填入，请确认季数、集数和下载选项后添加。';changed();}}/>{/if}
  {#if sourceMessage}<p class="notice success" role="status">{sourceMessage}</p>{/if}
  <label>番剧名称<input name="title" type="text" bind:value={title} required maxlength="200" placeholder="例如：葬送的芙莉莲" disabled={pending}/><span class="field-help">用于媒体库目录与文件命名</span></label>
  <label>RSS 地址<input name="feed_url" type="url" bind:value={feed} required maxlength="2048" placeholder="搜索选择后自动填写，也可手动粘贴 https://…" disabled={pending}/></label>
  <label>季数<input name="season" type="number" bind:value={season} min="1" max="99" required disabled={pending}/></label>
  <div class="episode-range"><label>起始集数<input name="start_episode" type="number" min="1" max="999" bind:value={start} required disabled={pending}/></label><label>结束集数<input name="end_episode" type="number" min="0" max="999" bind:value={end} disabled={pending}/><span class="field-help">0 表示不限</span></label></div>
  {#if subscription}<p class="field-help">已有剧集时，番剧名称与季数无法修改。可新建订阅来管理另一季。</p>{/if}
  <details bind:open={advanced} class="advanced-fields">
    <summary>筛选规则与元数据</summary>
    <div class="form-grid">
      <label>包含规则<input name="include" bind:value={include} type="text" maxlength="512" placeholder="留空时匹配番剧名称" disabled={pending}/><span class="field-help">支持正则表达式，可指定字幕组、清晰度</span></label>
      <label>排除规则<input name="exclude" bind:value={exclude} type="text" maxlength="512" placeholder="例如：720p|合集" disabled={pending}/></label>
      <label>TMDB 剧集 ID<input name="tmdb_id" bind:value={tmdb} type="number" min="0" max="2147483647" disabled={pending || metadataBusy}/><span class="field-help">可选，刮削封面与剧集信息需要配置 TMDB 凭据</span></label>
    </div>
    <MetadataPicker {title} value={tmdb} {season} configured={metadataConfigured} disabled={pending} onselect={(id) => {tmdb = id; changed();}} onseason={(value) => {season = value; changed();}} onbusy={(value) => metadataBusy = value}/>
  </details>
  <div><button class="button secondary" type="button" onclick={previewRules} disabled={pending || previewing || !title.trim() || !feed.trim()}>{previewing ? '正在读取 RSS…' : '预览 RSS 匹配'}</button><p class="field-help">仅检查当前规则，不保存订阅或提交下载。</p></div>
  {#if previewError}<p class="notice error" role="alert">{previewError}</p>{/if}
  {#if preview}<section class="feed-preview" aria-label="RSS 匹配预览"><p role="status">读取 {preview.scanned} 条 · 匹配 {preview.matched} 条 · 可发现 {preview.new} 集 · 已存在 {preview.existing} 条</p><ol>{#each preview.items as item}<li><strong>{item.title}</strong><span class:matched={item.matched}>{item.number ? '第 ' + item.number + ' 集 · ' : ''}{item.reason}</span></li>{/each}</ol>{#if preview.truncated}<p class="field-help">结果仅展示前 50 条，统计覆盖本次全部有效条目。</p>{/if}</section>{/if}
  <label class="check-row"><input name="enabled" type="checkbox" bind:checked={enabled} disabled={pending}/><span><strong>启用订阅</strong><small>关闭后暂停发现和核对剧集</small></span></label>
  <label class="check-row"><input name="auto_download" type="checkbox" bind:checked={auto} disabled={pending}/><span><strong>自动下载</strong><small>下载集数范围内的匹配单集，包括历史剧集</small></span></label>
  <div class="dialog-footer"><button class="button secondary" type="button" onclick={oncancel} disabled={pending || previewing}>取消</button><button class="button primary" type="submit" disabled={pending || previewing || metadataBusy || sourceBusy}>{pending ? '正在保存…' : subscription ? '保存修改' : '添加订阅'}</button></div>
</form>
