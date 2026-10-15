<script>
  import FileGlyph from './FileGlyph.svelte';
  let { entry, info, error='', src, onerror } = $props();
</script>
<div class="finder-preview-media">
  {#if error}<p class="inline-error" role="alert">{error}</p>
  {:else if !info}<p class="field-help" role="status">正在读取详情…</p>
  {:else if info.previewable && info.preview_kind==='image'}<img src={src} alt={entry.name} onerror={onerror}/>
  {:else if info.previewable && info.preview_kind==='video'}<video controls preload="none" src={src} onerror={onerror}><track kind="captions"/></video>
  {:else if info.previewable && info.preview_kind==='audio'}<audio controls preload="none" src={src} onerror={onerror}></audio>
  {:else}<FileGlyph {entry} size={90}/>{/if}
</div>
