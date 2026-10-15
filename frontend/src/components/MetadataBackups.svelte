<script>
  import { api, json } from '../lib/api.js';
  import { bytes, time } from '../lib/format.js';
  import Dialog from './Dialog.svelte';
  import TaskMonitor from './TaskMonitor.svelte';
  let { readOnly = true, onlibrarychange = () => {} } = $props();
  let items = $state(null), pending = $state(false), error = $state(''), confirming = $state(null), job = $state(''), taskRunning = $state(false), page = $state(0);
  let disposed = false;
  import { onDestroy } from 'svelte';
  onDestroy(() => {disposed = true;});
  async function load() {
    pending = true; error = '';
    try {const data = await api('/api/v1/anime/metadata/backups'); if (!disposed) {items = data.backups || []; page = Math.min(page,Math.max(0,Math.ceil(items.length/50)-1));}}
    catch (failure) {if (!disposed) error = failure.message;} finally {if (!disposed) pending = false;}
  }
  async function act() {
    if (!confirming || pending) return; pending = true; error = '';
    const {item,operation} = confirming;
    try {
      const path = '/api/v1/anime/metadata/backups/' + item.id;
      const result = await api(path + (operation === 'restore' ? '/restore' : ''),json(operation === 'restore' ? 'POST' : 'DELETE',operation === 'restore' ? {} : {sha256:item.sha256},{headers:{'X-Nasctl-Confirm':'1'}}));
      if (disposed) return; confirming = null;
      if (result.job_id) {job = result.job_id; taskRunning = true;} else await load();
    } catch (failure) {if (!disposed) error = failure.message;} finally {if (!disposed) pending = false;}
  }
</script>
<section class="panel settings-section metadata-backups">
  <div class="panel-heading"><h2>元数据旧版本</h2><button class="button secondary small" disabled={pending || taskRunning} onclick={load}>{pending ? '正在读取…' : items ? '刷新备份' : '查看元数据备份'}</button></div>
  <p class="field-help">替换 NFO 或封面前保存旧版本，视频和字幕保持原样。最多保留 1000 个版本、64 MiB；可先下载，再按需清理。</p>
  {#if error && !confirming}<p class="notice error" role="alert">{error}</p>{/if}
  {#if job}<TaskMonitor id={job} onsettled={() => {taskRunning = false; load(); onlibrarychange();}}/>{/if}
  {#if items}
    {#if !items.length}<p class="section-empty">暂无元数据旧版本。</p>{:else}
      <div class="table-wrap"><table><thead><tr><th>文件</th><th>保存时间</th><th>大小</th><th>操作</th></tr></thead><tbody>{#each items.slice(page*50,(page+1)*50) as item (item.id)}<tr><td class="path">{item.path}</td><td>{time(item.saved_at)}</td><td>{bytes(item.size)}</td><td><div class="button-group"><a class="text-button" href={'/api/v1/anime/metadata/backups/' + item.id + '/download'} download>下载</a><button class="text-button" disabled={readOnly || pending || taskRunning} onclick={() => {error = ''; confirming = {item,operation:'restore'};}}>恢复</button><button class="text-button" disabled={readOnly || pending || taskRunning} onclick={() => {error = ''; confirming = {item,operation:'delete'};}}>清理</button></div></td></tr>{/each}</tbody></table></div>
      <div class="pager"><span>{items.length} 个版本 · 第 {page+1} 页</span><div class="button-group"><button class="button secondary small" disabled={page === 0 || pending} onclick={() => page--}>上一页</button><button class="button secondary small" disabled={(page+1)*50 >= items.length || pending} onclick={() => page++}>下一页</button></div></div>
    {/if}
  {/if}
</section>
{#if confirming}<Dialog title={confirming.operation === 'restore' ? '恢复元数据版本' : '清理元数据备份'} onclose={() => {if (!pending) confirming = null;}}><p class="path">{confirming.item.path}</p><p>{confirming.operation === 'restore' ? '恢复这个版本，并保存当前文件为另一个备份。仅修改所选 NFO 或封面。' : '永久移除这个备份版本，无法恢复。当前媒体库文件保持原样，建议先下载保存。'}</p>{#if error}<p class="notice error" role="alert">{error}</p>{/if}<div class="dialog-footer"><button class="button secondary" disabled={pending} onclick={() => confirming = null}>取消</button><button class="button primary" disabled={pending} onclick={act}>{pending ? '正在处理…' : confirming.operation === 'restore' ? '确认恢复元数据' : '确认永久清理备份'}</button></div></Dialog>{/if}
