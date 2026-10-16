<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { time } from '../lib/format.js';
  import Icon from './Icon.svelte';
  import MetadataBackups from './MetadataBackups.svelte';
  let { settings, connection = null, pending = false, readOnly = true, oncheck, onlibrarychange } = $props();
</script>
{#if settings}
  <section class="panel">
    <div class="panel-heading"><h2>下载器连接</h2><button class="button secondary small" disabled={pending} onclick={oncheck}><Icon name="refresh" size={14}/>{pending ? '检查中…' : '检查连接'}</button></div>
    <div class="anime-settings-body">
      <dl class="key-values"><dt>下载器</dt><dd>qBittorrent</dd><dt>WebUI 地址</dt><dd class="path">{settings.qbittorrent_url}</dd><dt>连接状态</dt><dd>{#if connection}<span class="badge" class:complete={connection.connected} class:failed={!connection.connected}>{connection.connected ? '已连接' : '连接失败'}</span>{:else}<span class="muted">尚未检查</span>{/if}</dd>{#if connection?.connected}<dt>程序版本</dt><dd>{connection.version}</dd><dt>API 版本</dt><dd>{connection.api_version}</dd>{/if}{#if connection}<dt>检查时间</dt><dd>{time(connection.checked_at)}</dd>{/if}</dl>
      {#if connection?.error}<ErrorNotice message={connection.error}/>{/if}
      <p class="field-help">检查只读取版本信息，不新增、暂停或删除下载任务。</p>
    </div>
  </section>
  <section class="panel settings-section">
    <div class="panel-heading"><h2>调度与媒体库</h2><span class="badge" class:paused={!settings.auto_poll}>{settings.auto_poll ? '定时调度已开启' : '手动调度'}</span></div>
    <div class="anime-settings-body"><dl class="key-values"><dt>RSS 间隔</dt><dd>{settings.poll_interval_seconds} 秒</dd><dt>下载同步</dt><dd>{settings.sync_interval_seconds} 秒{!settings.auto_poll ? '（定时调度关闭，仅手动触发）' : ''}</dd><dt>RSS 读取上限</dt><dd>每个订阅 {settings.max_items_per_feed} 条</dd><dt>本地下载目录</dt><dd class="path">{settings.download_dir}</dd><dt>下载器内部目录</dt><dd class="path">{settings.qbittorrent_download_dir || settings.download_dir}</dd><dt>媒体库目录</dt><dd class="path">{settings.library_dir}</dd><dt>TMDB 凭据</dt><dd>{settings.tmdb_configured ? '已设置' : '未设置，仍可生成单集 NFO'}</dd></dl><p class="field-help">显示当前启动配置。修改 [hub.anime] 后重启生效；auto_poll 控制定时刷新和下载同步。暂停订阅会暂停该订阅的核对与归档，下载器中已提交的任务继续运行。</p></div>
  </section>
  <MetadataBackups {readOnly} {onlibrarychange}/>
  <section class="panel settings-section"><div class="panel-heading"><h2>追番记录导出</h2><a class="button secondary small" href="/api/v1/anime/history/export?limit=20000" download>导出全部追番记录</a></div><p class="field-help">导出订阅、剧集、观看进度和历史清理后的去重标记。不包含视频、下载器账号或 TMDB 令牌；RSS 和发布地址会保留。</p></section>
{/if}
