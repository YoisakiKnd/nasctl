<script>
  import { getContext, onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { bytes } from '../lib/format.js';
  import { desktop, saveDesktop, widgetList, engines } from '../lib/desktop.svelte.js';
  import Icon from './Icon.svelte';
  import Dialog from './Dialog.svelte';
  // The desktop's left column. System status is read every few seconds, and
  // only while the desktop is on screen: the NAS measures on request only.
  let { roots = [] } = $props();
  let status = $state(null), now = $state(new Date()), iface = $state(''), history = $state([]), settings = $state(false);
  const visible = getContext('nasctl-window-visible');
  let request, disposed = false;
  const selected = $derived(status?.network.find((item) => item.name === iface) || status?.network.find((item) => item.rx_bytes > 0) || status?.network[0]);
  const ring = (percent) => `${Math.max(0, Math.min(100, percent || 0)) * 2.51} 251`;
  const tone = (percent) => percent >= 90 ? 'hot' : percent >= 70 ? 'warm' : '';
  const rate = (value) => (value >= 1024 * 1024 ? (value / 1048576).toFixed(1) + ' MB' : value >= 1024 ? (value / 1024).toFixed(0) + ' KB' : Math.round(value || 0) + ' B') + '/s';
  const peak = $derived(Math.max(1024, ...history.map((point) => Math.max(point.rx, point.tx))));
  const line = (key) => history.map((point, index) => `${(index / 39) * 240},${60 - (point[key] / peak) * 56}`).join(' ');
  async function poll() {
    if (document.hidden || (visible && !visible())) return;
    request?.abort(); request = new AbortController();
    try {
      const next = await api('/api/v1/system/status', {timeoutMs:5000, signal:request.signal});
      if (disposed) return;
      status = next;
      const net = selected;
      if (net) history = [...history, {rx:net.rx_bytes_per_second, tx:net.tx_bytes_per_second}].slice(-40);
    } catch { /* The widgets keep their last reading. */ }
  }
  // Leaving the desktop cancels a reading still on its way.
  $effect(() => {if (visible && !visible()) request?.abort();});
  onMount(() => {
    poll();
    const timers = [setInterval(poll, 3000), setInterval(() => now = new Date(), 1000)];
    return () => {disposed = true; request?.abort(); timers.forEach(clearInterval);};
  });
</script>
<aside class="desktop-widgets" aria-label="小组件">
  {#if desktop.widgets.clock}<section class="glass widget-clock"><strong>{now.toLocaleTimeString('zh-CN', {hour:'2-digit', minute:'2-digit'})}</strong><span>{now.toLocaleDateString('zh-CN', {year:'numeric', month:'long', day:'numeric', weekday:'long'})}</span></section>{/if}

  {#if desktop.widgets.status}<section class="glass widget-status" aria-label="系统状态">
    <header><span>系统状态</span>{#if status?.temperature_c != null}<span class="widget-temp" class:hot={status.temperature_c >= 75}>{status.temperature_c.toFixed(0)}°C</span>{/if}</header>
    <div class="widget-rings">
      <div class={'ring ' + tone(status?.cpu.percent)}><svg viewBox="0 0 100 100" aria-hidden="true"><circle cx="50" cy="50" r="40"/><circle class="value" cx="50" cy="50" r="40" stroke-dasharray={ring(status?.cpu.percent)}/></svg><strong>{status ? Math.round(status.cpu.percent) + '%' : '—'}</strong><small>CPU{status ? `（${status.cpu.cores} 核）` : ''}</small></div>
      <div class={'ring ' + tone(status?.memory.percent)}><svg viewBox="0 0 100 100" aria-hidden="true"><circle cx="50" cy="50" r="40"/><circle class="value" cx="50" cy="50" r="40" stroke-dasharray={ring(status?.memory.percent)}/></svg><strong>{status ? Math.round(status.memory.percent) + '%' : '—'}</strong><small>内存{status ? `（${bytes(status.memory.total_bytes)}）` : ''}</small></div>
    </div>
  </section>{/if}

  {#if desktop.widgets.storage}<section class="glass widget-storage" aria-label="存储">
    <header><span>存储</span><a href="#files" aria-label="打开文件"><Icon name="chevron" size={15}/></a></header>
    {#each roots as root (root.id)}<div class="widget-disk">
      <span class="disk-glyph" class:offline={!root.available}><Icon name="disk" size={22}/></span>
      <div><strong>{root.label || root.id}</strong>{#if root.capacity}<small>{bytes(root.capacity.available_bytes)} 可用，共 {bytes(root.capacity.total_bytes)}</small><span class="widget-bar"><i class={tone(root.capacity.used_bytes / root.capacity.total_bytes * 100)} style:width={(root.capacity.used_bytes / root.capacity.total_bytes * 100) + '%'}></i></span>{:else}<small>{root.available ? '容量暂不可用' : '暂不可用'}</small>{/if}</div>
    </div>{:else}<p class="widget-empty">还没有存储目录</p>{/each}
  </section>{/if}

  {#if desktop.widgets.network && status?.network.length}<section class="glass widget-network" aria-label="网络">
    <header><span>网络</span>{#if status.network.length > 1}<select aria-label="网络接口" value={selected?.name} onchange={(event) => {iface = event.currentTarget.value; history = [];}}>{#each status.network as item (item.name)}<option value={item.name}>{item.name}</option>{/each}</select>{:else}<span>{selected?.name}</span>{/if}</header>
    <svg class="sparkline" viewBox="0 0 240 60" preserveAspectRatio="none" aria-hidden="true"><polyline class="rx" points={line('rx')}/><polyline class="tx" points={line('tx')}/></svg>
    <footer><span class="rx"><Icon name="back" size={12} class="down"/>{rate(selected?.rx_bytes_per_second)}</span><span class="tx"><Icon name="back" size={12} class="up"/>{rate(selected?.tx_bytes_per_second)}</span></footer>
  </section>{/if}

  <button class="glass widget-settings" onclick={() => settings = true}><span>小组件设置</span><Icon name="chevron" size={15}/></button>
</aside>
{#if settings}<Dialog title="小组件设置" description="只影响这台设备上的浏览器。" onclose={() => settings = false}>
  <div class="switch-list">{#each widgetList as item (item.id)}<label class="switch-row"><span><strong>{item.label}</strong></span><input type="checkbox" role="switch" class="switch" bind:checked={desktop.widgets[item.id]} onchange={saveDesktop}/></label>{/each}</div>
  <label class="field desktop-engine">搜索框默认搜索<select aria-label="默认搜索" bind:value={desktop.engine} onchange={saveDesktop}>{#each engines as item (item.id)}<option value={item.id}>{item.label}</option>{/each}</select></label>
  <p class="field-help">壁纸在“设置 › 账号与外观”中更换。</p>
  <div class="dialog-footer"><button class="button primary" onclick={() => settings = false}>完成</button></div>
</Dialog>{/if}
