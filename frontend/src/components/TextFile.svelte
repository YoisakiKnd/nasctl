<script>
  import { onDestroy } from 'svelte';
  import { api, query } from '../lib/api.js';
  import { bytes } from '../lib/format.js';
  import ErrorNotice from './ErrorNotice.svelte';
  // compact: a short read-only excerpt for the details pane. Otherwise the
  // whole (bounded) text with editing when the location allows writes.
  let { root, path, name, compact = false, writable = false, startEditing = false, dirty = $bindable(false), onclose, onsaved, download } = $props();
  let data = $state(null), error = $state(''), editing = $state(false), draft = $state(''), saving = $state(false), saveError = $state(null), notice = $state('');
  let controller, area = $state();
  dirty = false;
  async function load() {
    controller?.abort(); controller = new AbortController(); error = ''; saveError = null;
    try {
      data = await api(query('/api/v1/files/text', { root, path }), { signal: controller.signal });
      draft = data.text.content; dirty = false;
      if (startEditing && canEdit()) edit();
    } catch (failure) { if (failure.name !== 'AbortError') error = failure.message; }
  }
  load();
  onDestroy(() => controller?.abort());
  const canEdit = () => writable && data?.editable;
  async function edit() { editing = true; notice = ''; await Promise.resolve(); area?.focus(); area?.setSelectionRange(0, 0); if (area) area.scrollTop = 0; }
  function cancel() { if (dirty && !confirm('放弃未保存的修改？')) return; draft = data.text.content; dirty = false; editing = false; saveError = null; }
  async function save() {
    if (saving || !dirty) return;
    saving = true; saveError = null; notice = '';
    try {
      const result = await api(query('/api/v1/files/text', { root, path, revision: data.text.revision }), { method: 'PUT', headers: { 'Content-Type': 'text/plain; charset=utf-8', 'X-Nasctl-Confirm': '1' }, body: draft });
      data = { ...data, text: { ...data.text, content: draft, revision: result.result.revision, size: result.result.entry?.size ?? data.text.size } };
      dirty = false; editing = false; notice = '已保存。';
      onsaved?.();
    } catch (failure) { saveError = failure; }
    finally { saving = false; }
  }
  function keys(event) {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 's') { event.preventDefault(); save(); }
    // Tab inserts a tab instead of leaving the editor; Escape still closes.
    if (event.key === 'Tab' && !event.shiftKey && !event.ctrlKey && !event.altKey) {
      event.preventDefault();
      const { selectionStart: a, selectionEnd: b } = area;
      draft = draft.slice(0, a) + '\t' + draft.slice(b); dirty = true;
      queueMicrotask(() => area.setSelectionRange(a + 1, a + 1));
    }
  }
  const lines = $derived(data ? (editing ? draft : data.text.content).split("\n").length : 0);
</script>
{#if compact}
  <div class="text-excerpt">
    {#if error}<p class="inline-error" role="alert">{error}</p>
    {:else if !data}<p class="field-help" role="status">正在读取…</p>
    {:else if data.text.binary}<p class="field-help">这个文件不是 UTF-8 文本，无法预览。</p>
    {:else if !data.text.content}<p class="field-help">空文件</p>
    {:else}<pre aria-label="文本预览">{data.text.content.slice(0, 2000)}</pre>{/if}
  </div>
{:else}
  <div class="text-file">
    {#if error}<ErrorNotice message={error} onretry={load}/>
    {:else if !data}<p class="field-help" role="status">正在读取…</p>
    {:else if data.text.binary}<p class="notice">这个文件不是 UTF-8 文本（可能是二进制文件或其他编码），请下载后用本地应用打开。</p>
    {:else}
      {#if data.text.truncated}<p class="notice">文件较大（{bytes(data.text.size)}），这里只显示前 {bytes(data.limit)}，不能在网页里编辑。</p>{/if}
      {#if editing}<textarea class="text-editor" aria-label="编辑文本" spellcheck="false" bind:this={area} bind:value={draft} oninput={(event) => { dirty = event.currentTarget.value !== data.text.content; }} onkeydown={keys} disabled={saving}></textarea>
      {:else}<pre class="text-view" aria-label="文本内容">{data.text.content || ' '}</pre>{/if}
      {#if saveError}
        {#if saveError.code === 'source_changed'}<div class="notice error" role="alert">文件在你打开后被修改过，为避免覆盖，这次没有保存。可以先复制你的修改，再<button class="text-button" onclick={() => { if (confirm('重新载入会丢弃你的修改，继续？')) { editing = false; load(); } }}>重新载入</button>最新内容。</div>
        {:else}<ErrorNotice message={saveError.message}/>{/if}
      {/if}
      {#if notice}<p class="field-help" role="status">{notice}</p>{/if}
    {/if}
    <footer class="dialog-footer">
      <span class="muted">{#if data && !data.text.binary}{lines} 行 · {bytes(editing ? new Blob([draft]).size : data.text.size)}{#if dirty} · 未保存{/if}{:else if data}{bytes(data.text.size)}{/if}</span>
      {#if editing}
        <button class="button secondary" disabled={saving} onclick={cancel}>取消</button>
        <button class="button primary" disabled={saving || !dirty} onclick={save} title="Ctrl+S">{saving ? '正在保存…' : '保存'}</button>
      {:else}
        <button class="button secondary" onclick={onclose}>关闭</button>
        {#if download}<a class="button secondary" href={download} download={name}>下载</a>{/if}
        {#if canEdit()}<button class="button primary" onclick={edit}>编辑</button>
        {:else if data && !data.text.binary && !data.text.truncated && !writable}<span class="field-help">此位置只读</span>{/if}
      {/if}
    </footer>
  </div>
{/if}
