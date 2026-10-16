<script>
  import ErrorNotice from '../../components/ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api } from '../../lib/api.js';
  import { time } from '../../lib/format.js';
  import Icon from '../../components/Icon.svelte';
  let approvals = $state([]), audit = $state([]), error = $state(''), loading = $state(true);
  let disposed = false, request;
  async function load() {
    request?.abort(); const current = new AbortController(); request = current; const options = {signal:current.signal}; loading = true;
    const results = await Promise.allSettled([api('/api/v1/approvals',options),api('/api/v1/audit?limit=50',options)]);
    if (disposed || current.signal.aborted || request !== current) return;
    const [a,h] = results;
    if (a.status === 'fulfilled') approvals = a.value.approvals || [];
    if (h.status === 'fulfilled') audit = h.value.entries || [];
    error = results.filter((result) => result.status === 'rejected').map((result) => result.reason.summary || result.reason.message).join('；'); loading = false;
  }
  onMount(() => {load(); return () => {disposed = true; request?.abort();};});
</script>
<header class="page-heading"><div><h1>操作记录</h1><p class="muted">最近的操作与授权，便于回溯。</p></div><button class="button secondary" disabled={loading} onclick={load}><Icon name="refresh" size={16}/>刷新</button></header>
{#if error}<ErrorNotice message={error}/>{/if}
<section class="panel settings-section"><div class="panel-heading"><h2>最近操作</h2><span class="muted">最近 50 条</span></div>{#if !audit.length}<p class="section-empty">还没有操作记录。</p>{:else}<div class="table-wrap"><table><thead><tr><th>时间</th><th>调用者</th><th>操作</th><th>目标</th><th>结果</th></tr></thead><tbody>{#each audit as entry}<tr><td>{time(entry.at)}</td><td>{entry.principal}</td><td class="mono">{entry.op || entry.action}</td><td class="path">{entry.target || '—'}</td><td><span class="badge" class:failed={['refused','failed'].includes(entry.outcome)}>{entry.outcome}</span>{#if entry.note}<small class="muted">{entry.note}</small>{/if}</td></tr>{/each}</tbody></table></div>{/if}</section>
<section class="panel settings-section"><div class="panel-heading"><h2>授权记录</h2><span class="muted">{approvals.length} 条</span></div>{#if !approvals.length}<p class="section-empty">暂无授权记录。</p>{:else}<div class="table-wrap"><table><thead><tr><th>操作</th><th>调用者</th><th>目标</th><th>到期时间</th><th>使用状态</th></tr></thead><tbody>{#each approvals as approval}<tr><td class="mono">{approval.op}</td><td>{approval.principal}</td><td>{approval.target || '—'}</td><td>{time(approval.expires_at)}</td><td>{approval.used_at ? '已使用' : '未使用'}</td></tr>{/each}</tbody></table></div>{/if}</section>
