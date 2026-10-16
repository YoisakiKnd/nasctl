<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { onDestroy } from 'svelte';
  import { api, query as urlQuery } from '../lib/api.js';
  let { title = '', value = 0, season = 1, configured = false, disabled = false, onselect, onseason, onbusy = () => {} } = $props();
  let query = $state(''), searched = $state(''), page = $state(1), results = $state(null), selected = $state(null), busy = $state(false), error = $state('');
  let request, generation = 0;
  $effect(() => {if (selected && selected.id !== value) selected = null;});
  async function search(next = 1) {
    if (busy || disabled) return;
    const text = next === 1 ? (query.trim() || title.trim()) : searched;
    if (!text) return;
    const current = ++generation; request?.abort(); request = new AbortController(); busy = true; onbusy(true); error = '';
    try {const result = await api(urlQuery('/api/v1/anime/metadata/search',{q:text,page:next}),{signal:request.signal}); if (current === generation) { results = result; searched = text; page = next; }}
    catch (failure) {if (current === generation && failure.name !== 'AbortError') error = failure.message;}
    finally {if (current === generation) {busy = false; onbusy(false);}}
  }
  async function choose(id) {
    if (busy || disabled) return;
    const current = ++generation; request?.abort(); request = new AbortController(); busy = true; onbusy(true); error = '';
    try {const result = await api('/api/v1/anime/metadata/series/' + id,{signal:request.signal}); if (current === generation) {selected = result; results = null; onselect(result.id);}}
    catch (failure) {if (current === generation && failure.name !== 'AbortError') error = failure.message;}
    finally {if (current === generation) {busy = false; onbusy(false);}}
  }
  onDestroy(() => {++generation; request?.abort();});
</script>
<section class="metadata-picker" aria-label="TMDB 元数据选择" oninput={(event) => event.stopPropagation()}>
  {#if configured}<label>查找 TMDB 剧集<input aria-label="TMDB 搜索名称" bind:value={query} placeholder={title || '输入番剧名称'} maxlength="200" disabled={disabled || busy} onkeydown={(event) => {if (event.key === 'Enter') {event.preventDefault(); search();}}}/></label>
    <div class="button-group"><button class="button secondary small" type="button" disabled={disabled || busy || !(query.trim() || title.trim())} onclick={() => search()}>{busy ? '正在查询…' : '搜索元数据'}</button>{#if value > 0}<button class="text-button" type="button" disabled={disabled || busy} onclick={() => choose(value)}>查看当前 ID 的季信息</button>{/if}</div>
    {#if results}<div class="metadata-results"><p class="field-help">“{searched}” · 第 {page} 页 · {results.results.length} 项</p>{#if !results.results.length}<p class="muted">没有找到匹配剧集，可换一个名称或手动填写 ID。</p>{/if}<ul>{#each results.results as item (item.id)}<li><div><strong>{item.name}</strong><small>{item.original_name} · {item.first_air_date || '首播日期未知'} · TMDB {item.id}</small>{#if item.overview}<p>{item.overview}</p>{/if}</div><button class="button secondary small" type="button" disabled={disabled || busy} onclick={() => choose(item.id)}>{value === item.id ? '查看' : '选择'}</button></li>{/each}</ul>{#if results.truncated}<p class="field-help">本页达到 20 项显示上限，请使用更具体的名称。</p>{/if}<div class="button-group"><button class="text-button" type="button" disabled={disabled || busy || page <= 1} onclick={() => search(page - 1)}>上一页结果</button><button class="text-button" type="button" disabled={disabled || busy || !results.has_more} onclick={() => search(page + 1)}>下一页结果</button></div></div>{/if}
    {#if selected}<div class="metadata-selection"><p><strong>已选择：{selected.name}</strong> · <a href={'https://www.themoviedb.org/tv/' + selected.id} target="_blank" rel="noreferrer">TMDB {selected.id}</a></p>{#if selected.seasons.length}<label>TMDB 季数<select aria-label="TMDB 季数选择" value={season} disabled={disabled || busy} onchange={(event) => onseason(Number(event.currentTarget.value))}>{#if !selected.seasons.some((item) => item.season_number === season)}<option value={season}>当前第 {season} 季（TMDB 未列出）</option>{/if}{#each selected.seasons as item}<option value={item.season_number}>第 {item.season_number} 季 · {item.name} · {item.episode_count} 集{item.air_date ? ' · ' + item.air_date : ''}</option>{/each}</select></label>{:else}<p class="field-help">TMDB 暂未提供第 1–99 季的信息，请核对上方季数。</p>{/if}<p class="field-help">只更新 ID 和你选择的季数，保留番剧名称与 RSS 规则。保存订阅后生效。</p></div>{/if}
  {:else}<p class="field-help">未配置 TMDB 凭据。仍可手动填写 ID；设置 tmdb_token 并重启后可搜索和刮削。</p>{/if}
  {#if error}<ErrorNotice message={error}/>{/if}
</section>
