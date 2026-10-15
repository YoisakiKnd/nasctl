<script>
  import { onMount, onDestroy, tick } from 'svelte';
  import { api, json } from '../lib/api.js';
  import { duration } from '../lib/format.js';
  import Dialog from './Dialog.svelte';
  import Icon from './Icon.svelte';
  let { episode, readOnly = false, onclose } = $props();
  let playback = $state(null), error = $state(''), saveError = $state(''), saved = $state(''), closing = $state(false);
  let subtitleError = $state('');
  let video = $state(), subtitle = $state(''), ready = false, disposed = false, saveRequest, lastAttempt = 0, loadRequest;
  const subtitleURL = (name) => `/api/v1/anime/episodes/${episode.id}/subtitles/${encodeURIComponent(name)}`;
  onMount(() => {
    loadRequest = new AbortController();
    api(`/api/v1/anime/episodes/${episode.id}/playback`, { signal: loadRequest.signal })
      .then((result) => { if (!disposed) playback = result; })
      .catch((failure) => { if (!disposed && failure.name !== 'AbortError') error = failure.message; });
  });
  function loaded() {
    const position = playback?.episode.watch_position || 0;
    if (!playback?.episode.watched && position > 0 && Number.isFinite(video.duration)) video.currentTime = Math.min(position, Math.max(0, video.duration - 1));
    ready = true;
  }
  async function changeSubtitle(event) {
    subtitle = event.currentTarget.value;
    subtitleError = '';
    await tick();
    if (video) for (let index=0;index<video.textTracks.length;index++) video.textTracks[index].mode = subtitle ? 'showing' : 'disabled';
  }
  async function persist(force = false) {
    if (readOnly || !ready || !video || !Number.isFinite(video.duration) || video.duration <= 0 || video.duration > 86400) return;
    const currentVideo = video;
    while (saveRequest) { if (!force) return; await saveRequest; }
    if (!force && Date.now() - lastAttempt < 15000) return;
    const position = Math.min(Math.max(currentVideo.currentTime, 0), currentVideo.duration), total = currentVideo.duration;
    lastAttempt = Date.now();
    saveRequest = api(`/api/v1/anime/episodes/${episode.id}/playback`, json('PUT', { position, duration: total }, {keepalive:true}))
      .then((result) => { if (!disposed) { saveError = ''; saved = result.watched ? '已看完 · 进度已保存' : `进度已保存至 ${duration(result.position)}`; } return true; })
      .catch((failure) => { if (!disposed) saveError = '观看进度未保存：' + failure.message; return false; })
      .finally(() => { saveRequest = null; });
    return await saveRequest;
  }
  async function close() {
    if (closing) return;
    closing = true;
    if (await persist(true) === false) { closing = false; return; }
    onclose();
  }
  onDestroy(() => { disposed = true; loadRequest?.abort(); if (!closing) persist(true); });
</script>
<Dialog title={`${episode.series} · 第 ${episode.number} 集`} wide onclose={close}>
  {#if error}<p class="notice error" role="alert">{error}</p>
  {:else if !playback}<p class="loading-state" role="status">正在读取播放信息…</p>
  {:else}
    <video bind:this={video} controls playsinline preload="metadata" src={`/api/v1/anime/episodes/${episode.id}/video`} onloadedmetadata={loaded} ontimeupdate={() => { if (!video.paused) persist(); }} onpause={() => persist(true)} onended={() => persist(true)} onerror={() => error = '浏览器无法播放此文件。可下载原文件，或使用支持此编码的播放器。'}>
      <track kind="captions" src={subtitle ? subtitleURL(subtitle) : undefined} label="外挂字幕" srclang="und" default={Boolean(subtitle)} onload={(event) => {event.currentTarget.track.mode = subtitle ? 'showing' : 'disabled';}} onerror={() => subtitleError = '字幕加载失败，请检查文件编码；仍可下载原字幕。'}/>
    </video>
    {#if playback.episode.watch_position > 0 && !playback.episode.watched}<p class="field-help">将从 {duration(playback.episode.watch_position)} 继续播放。</p>{/if}
    {#if playback.subtitles.length}<div class="player-subtitles"><label>播放字幕<select value={subtitle} onchange={changeSubtitle}><option value="">不显示外挂字幕</option>{#each playback.subtitles.filter((item) => item.playable) as item}<option value={item.name}>{item.label} · {item.format.toUpperCase()}</option>{/each}</select></label><div class="subtitle-downloads">{#each playback.subtitles as item}<a class="text-button" href={subtitleURL(item.name) + '?download=1'} download={item.name}>下载 {item.label}（{item.format.toUpperCase()}）</a>{/each}</div><p class="field-help">浏览器可显示 UTF-8 的 SRT／VTT；ASS 等字幕可下载到外部播放器使用。</p></div>{/if}
    {#if playback.truncated}<p class="notice warning">目录或字幕数量达到读取上限，仅展示部分字幕。</p>{/if}
    {#if subtitleError}<p class="notice error" role="alert">{subtitleError}</p>{/if}
    <p class="field-help" role="status">{readOnly ? '只读模式：播放不会保存观看进度。' : saved || '播放过程中每 15 秒、暂停和关闭时保存进度；看到 95% 后标记为已看完。'}</p>
  {/if}
  {#if saveError}<div class="notice error" role="alert"><p>{saveError}</p><button class="text-button" onclick={() => {closing = true; onclose();}}>放弃本次进度并关闭</button></div>{/if}
  <p class="muted">直接播放原文件，编码支持取决于浏览器。</p>
  <div class="dialog-footer"><a class="button secondary" href={`/api/v1/anime/episodes/${episode.id}/video`} download={episode.library_path.split('/').pop()}><Icon name="download" size={16}/>下载原文件</a><button class="button secondary" disabled={closing} onclick={close}>{closing ? '正在保存…' : '关闭播放器'}</button></div>
</Dialog>
