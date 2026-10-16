<script>
  import ErrorNotice from '../components/ErrorNotice.svelte';
  import { bytes, time, modeLabels, jobLabels } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  let { snapshot = null, error = '', onrefresh, loading = false, jobsAvailable = true, canConfigure = false } = $props();
  const resources = $derived(snapshot?.resources);
  const roots = $derived(snapshot?.files?.roots || []);
  const jobs = $derived(snapshot?.running || []);
</script>
<header class="page-heading"><div><h1>概览</h1><p class="muted">设备状态、存储与任务一览。</p></div><button class="button secondary" onclick={onrefresh} disabled={loading}><Icon name="refresh" size={16}/>刷新状态</button></header>
{#if error}<ErrorNotice message={error}/>{/if}
{#if !snapshot}<div class="loading-state" role="status">正在读取设备状态…</div>{:else}
<section class="stat-strip overview-stats" aria-label="设备概况">
  <div><span>运行模式</span><strong class="text-stat">{modeLabels[snapshot.mode?.mode] || '未知'}</strong></div>
  <div><span>可用内存</span><strong class="text-stat">{resources ? bytes(resources.mem_available_kb * 1024) : '暂不可用'}</strong></div>
  <div><span>已连接存储</span><strong>{roots.filter((root) => root.available).length}<small>/ {roots.length}</small></strong></div>
  {#if jobsAvailable}<div><span>进行中任务</span><strong>{jobs.length}<small>项</small></strong></div>{/if}
</section>
{#if snapshot.warnings?.length}<div class="notice warning" id="overview-warnings" role="status"><Icon name="warning" size={18}/><div>{#each snapshot.warnings as warning}<p>{warning}</p>{/each}</div></div>{/if}
<div class="dashboard-grid">
  <section class="panel"><div class="panel-heading"><h2>快捷入口</h2><span class="muted">常用管理</span></div><div class="quick-links"><a href="#admin/anime"><span class="feature-icon accent"><Icon name="anime" size={23}/></span><div><strong>追番管理</strong><small>订阅、下载器与媒体库</small></div><Icon name="chevron" size={17}/></a><a href="#admin/album"><span class="feature-icon"><Icon name="album" size={23}/></span><div><strong>相册管理</strong><small>照片目录与限量处理</small></div><Icon name="chevron" size={17}/></a>{#if canConfigure}<a href="#admin/system"><span class="feature-icon"><Icon name="settings" size={23}/></span><div><strong>系统设置</strong><small>存储目录与运行模式</small></div><Icon name="chevron" size={17}/></a>{:else}<a href="#files"><span class="feature-icon"><Icon name="folder" size={23}/></span><div><strong>文件管理</strong><small>浏览和整理你的存储空间</small></div><Icon name="chevron" size={17}/></a>{/if}</div></section>
  <section class="panel"><div class="panel-heading"><h2>存储空间</h2><a class="text-button" href="#files">查看文件<Icon name="chevron" size={14}/></a></div>{#if !roots.length}<div class="compact-empty"><Icon name="disk" size={32}/><p>还没有添加存储目录</p><small>配置存储根目录后，可在文件管理中访问。</small></div>{:else}<ul class="storage-list">{#each roots as root}<li><span class="feature-icon"><Icon name="disk"/></span><div><strong>{root.label || root.id}</strong><p class="muted path">{root.available ? root.path : root.note || '目录暂不可用'}</p>{#if root.capacity}<p class="muted">{bytes(root.capacity.available_bytes)} 可用 / {bytes(root.capacity.total_bytes)} · 文件系统已用 {root.capacity.usage_percent.toFixed(1)}%</p><progress class="capacity-meter" value={root.capacity.used_bytes} max={root.capacity.total_bytes} aria-label={(root.label||root.id)+' 文件系统空间使用率'}></progress>{#if roots.find((item)=>item.id!==root.id&&item.capacity?.volume_id===root.capacity.volume_id)}<small class="muted">与其他目录共用文件系统，容量不重复相加。</small>{/if}{:else if root.available}<p class="muted">容量暂不可用</p>{/if}</div><span class="badge" class:failed={!root.available}>{root.available ? '已连接' : '不可用'}</span></li>{/each}</ul>{/if}</section>
  {#if jobsAvailable}<section class="panel full-width"><div class="panel-heading"><h2>进行中任务</h2><a class="text-button" href="#admin/jobs">任务中心<Icon name="chevron" size={14}/></a></div>{#if !jobs.length}<div class="compact-empty horizontal"><Icon name="check" size={25}/><div><strong>此刻没有任务运行</strong><p class="muted">设备可以安静地休息一会儿。</p></div></div>{:else}<div class="table-wrap"><table><thead><tr><th>任务</th><th>状态</th><th>发起者</th></tr></thead><tbody>{#each jobs as job}<tr><td>{job.op}</td><td><span class="badge">{jobLabels[job.state] || job.state}</span></td><td>{job.principal}</td></tr>{/each}</tbody></table></div>{/if}</section>{/if}
  {#if jobsAvailable&&snapshot.attention?.length}<section class="panel full-width"><div class="panel-heading"><h2>需要留意</h2><span class="badge failed">{snapshot.attention.length} 项</span></div>{#each snapshot.attention as job}<div class="attention-row"><strong>{job.op}</strong><span>{jobLabels[job.state] || job.state}</span><p class="muted">{job.error || job.stop_reason || '打开任务中心查看详情。'}</p></div>{/each}</section>{/if}
</div>
{#if snapshot.resource_error}<p class="page-note"><Icon name="warning" size={15}/>资源监控暂不可用；这里不会用零值代替未知状态。<span class="sr-only">{snapshot.resource_error}</span></p>{/if}
<footer class="page-note" id="overview-stamp">更新于 {time(snapshot.data_as_of)}<span>·</span>nasctl {snapshot.version}</footer>
{/if}
