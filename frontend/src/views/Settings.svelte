<script>
  import { onMount } from 'svelte';
  import { api, json } from '../lib/api.js';
  import { modeLabels, time } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import Dialog from '../components/Dialog.svelte';
  import NASSettings from '../components/NASSettings.svelte';
  import UserAccounts from '../components/UserAccounts.svelte';
  let { onchange,auth=null,onaccountschange } = $props();
  let modes = $state(null), approvals = $state([]), operations = $state([]), audit = $state([]), error = $state(''), loading = $state(true), pending = $state(false), confirmMode = $state(null), message = $state('');
  let legacy = $state(null), legacyError = $state(''), legacyLoading = $state(false);
  let disposed = false, request;
  async function loadLegacy() {
    legacyLoading = true; legacyError = '';
    try { const data = await api('/api/v1/media/status'); if (!disposed) legacy = data; }
    catch (failure) { if (!disposed) legacyError = failure.message; }
    finally { if (!disposed) legacyLoading = false; }
  }
  async function load() {
    request?.abort(); const current = new AbortController(); request = current; const options = {signal:current.signal}; loading = true;
    const results = await Promise.allSettled([api('/api/v1/modes',options),api('/api/v1/approvals',options),api('/api/v1/ops',options),api('/api/v1/audit?limit=50',options)]);
    if (disposed || current.signal.aborted || request !== current) return;
    const [m,a,o,h] = results;
    if (m.status === 'fulfilled') modes = m.value;
    if (a.status === 'fulfilled') approvals = a.value.approvals || [];
    if (o.status === 'fulfilled') operations = o.value.operations || [];
    if (h.status === 'fulfilled') audit = h.value.entries || [];
    error = results.filter((result) => result.status === 'rejected').map((result) => result.reason.message).join('；'); loading = false;
  }
  async function change(mode, clear = false) {
    if (pending) return; pending = true; error = ''; message = '';
    try {
      await api('/api/v1/modes' + (clear ? '/' + mode : ''),clear ? {method:'DELETE'} : json('POST',{mode,duration:mode === 'manual_pause' ? '' : mode === 'watching' ? '2h' : '1h',reason:'由控制台操作者设置'}));
      if (disposed) return;
      confirmMode = null; message = '运行模式已更新。'; await load(); onchange?.();
    } catch (failure) { if (!disposed) error = failure.message; }
    finally { if (!disposed) pending = false; }
  }
  onMount(() => {load(); return () => {disposed = true; request?.abort();};});
</script>
<header class="page-heading"><div><p class="eyebrow">偏好与控制</p><h1>设置</h1><p class="muted">掌握运行节奏，查看权限与操作记录。</p></div><button class="button secondary" disabled={loading || pending} onclick={load}><Icon name="refresh" size={16}/>刷新</button></header>
{#if error}<div class="notice error" role="alert">{error}</div>{/if}{#if message}<div class="notice success" role="status">{message}</div>{/if}
<NASSettings/>
<UserAccounts {auth} onchange={onaccountschange}/>
<section class="panel"><div class="panel-heading"><h2>运行模式</h2><span class="badge">{modeLabels[modes?.effective?.mode] || '读取中'}</span></div><div class="mode-options"><div><strong>观影模式</strong><p class="muted">暂停重任务 2 小时，让设备专注于播放。</p></div><button class="button secondary small" disabled={pending || !modes} onclick={() => confirmMode = 'watching'}>开启观影</button><div><strong>维护模式</strong><p class="muted">设置 1 小时的维护窗口，任务仍受资源保护。</p></div><button class="button secondary small" disabled={pending || !modes} onclick={() => confirmMode = 'maintenance'}>开启维护</button><div><strong>手动暂停</strong><p class="muted">持续暂停重任务，直到你主动恢复。</p></div><button class="button secondary small" disabled={pending || !modes} onclick={() => confirmMode = 'manual_pause'}>暂停任务</button></div>{#if modes?.states?.length}<div class="active-modes">{#each modes.states as mode}<div><span><strong>{modeLabels[mode.mode] || mode.mode}</strong><small class="muted">{mode.expires_at ? '截止 ' + time(mode.expires_at) : '持续生效'} · {mode.source}</small></span>{#if ['watching','maintenance','manual_pause'].includes(mode.mode)}<button class="text-button" disabled={pending} onclick={() => change(mode.mode,true)}>解除</button>{/if}</div>{/each}</div>{/if}</section>
<section class="panel settings-section"><div class="panel-heading"><h2>授权记录</h2><span class="muted">{approvals.length} 条</span></div>{#if !approvals.length}<p class="section-empty">暂无授权记录。</p>{:else}<div class="table-wrap"><table><thead><tr><th>操作</th><th>调用者</th><th>目标</th><th>到期时间</th><th>使用状态</th></tr></thead><tbody>{#each approvals as approval}<tr><td class="mono">{approval.op}</td><td>{approval.principal}</td><td>{approval.target || '—'}</td><td>{time(approval.expires_at)}</td><td>{approval.used_at ? '已使用' : '未使用'}</td></tr>{/each}</tbody></table></div>{/if}</section>
<section class="panel settings-section"><div class="panel-heading"><h2>最近操作</h2><span class="muted">最近 50 条</span></div>{#if !audit.length}<p class="section-empty">还没有操作记录。</p>{:else}<div class="table-wrap"><table><thead><tr><th>时间</th><th>调用者</th><th>操作</th><th>目标</th><th>结果</th></tr></thead><tbody>{#each audit as entry}<tr><td>{time(entry.at)}</td><td>{entry.principal}</td><td class="mono">{entry.op || entry.action}</td><td class="path">{entry.target || '—'}</td><td><span class="badge" class:failed={['refused','failed'].includes(entry.outcome)}>{entry.outcome}</span>{#if entry.note}<small class="muted">{entry.note}</small>{/if}</td></tr>{/each}</tbody></table></div>{/if}</section>
<details class="panel settings-section"><summary class="panel-heading">旧版媒体库兼容<span class="muted">可选</span></summary><div class="legacy-panel"><p class="muted">如果仍保留 IWantWatchAnime 的数据库，可通过只读适配器查看。</p><button class="button secondary small" disabled={legacyLoading} onclick={loadLegacy}>{legacyLoading ? '正在读取…' : '读取旧版媒体库'}</button>{#if legacyError}<p class="notice error">{legacyError}</p>{/if}{#if legacy}<p class="muted">{legacy.configured ? '旧版媒体库已连接，仅提供读取。' : '旧版媒体库未配置。原生追番不需要这个数据库。'}</p>{#if legacy.configured}<dl class="key-values"><dt>媒体库</dt><dd>{legacy.library_count ?? '未知'}</dd><dt>订阅</dt><dd>{legacy.subscription_count ?? '未知'}</dd><dt>任务</dt><dd>{legacy.task_count ?? '未知'}</dd></dl><ul>{#each legacy.library || [] as item}<li>{item.title || item.path || '未命名条目'}</li>{/each}</ul>{/if}{/if}</div></details>
<details class="panel settings-section"><summary class="panel-heading">操作能力与限制<span class="muted">高级</span></summary><div class="table-wrap"><table><thead><tr><th>操作</th><th>权限级别</th><th>审批策略</th><th>超时</th></tr></thead><tbody>{#each operations as operation}<tr><td class="mono">{operation.id}</td><td>{operation.class}</td><td>{operation.approval}</td><td>{operation.timeout}</td></tr>{/each}</tbody></table></div></details>
{#if confirmMode}<Dialog title={'开启' + modeLabels[confirmMode]} onclose={() => {if (!pending) confirmMode = null;}}><p>{confirmMode === 'manual_pause' ? '重任务将暂停，重启后仍保持暂停，需要你主动解除。已经提交的下载仍由下载器管理。' : confirmMode === 'watching' ? '接下来的 2 小时暂停重任务，已提交的下载由下载器继续管理。' : '设置 1 小时维护窗口；任务依然必须通过资源检查。'}</p><div class="dialog-footer"><button class="button secondary" disabled={pending} onclick={() => confirmMode = null}>取消</button><button class="button primary" disabled={pending} onclick={() => change(confirmMode)}>{pending ? '正在设置…' : '确认开启'}</button></div>{#if error}<p class="notice error" role="alert">{error}</p>{/if}</Dialog>{/if}
