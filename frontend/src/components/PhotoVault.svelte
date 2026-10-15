<script>
  import { getContext, onMount } from 'svelte';
  import { api, json, query } from '../lib/api.js';
  import Icon from './Icon.svelte';
  import PhotoViewer from './PhotoViewer.svelte';
  let { readOnly = true, candidate = null, oncancel, onmoved } = $props();
  let state = $state(null), listing = $state(null), selected = $state(null), page = $state(1), password = $state(''), repeat = $state(''), newPassword = $state(''), changing = $state(false), pending = $state(false), error = $state(''), message = $state(''), failedThumbs = $state({});
  const visible = getContext('nasctl-window-visible');
  let disposed = false, request, generation = 0, timer;
  const confirm = {'X-Nasctl-Confirm':'1'};
  async function refresh() {
    const current = ++generation; request?.abort(); request = new AbortController();
    try {
      const status = await api('/api/v1/album/vault/status',{signal:request.signal});
      if (disposed || generation !== current) return; state = status;
      clearTimeout(timer);
      if (!status.unlocked) {listing = null; selected = null; return;}
      timer = setTimeout(() => lock(), Math.max(0,status.expires_at*1000-Date.now()));
      const result = await api(query('/api/v1/album/vault/media',{page}),{signal:request.signal});
      if (!disposed && generation === current) listing = {...result,items:result.items.map((item) => ({...item,vault:true}))};
    } catch (failure) {if (!disposed && current === generation && failure.name !== 'AbortError') {error = failure.message; if (failure.code === 'vault_locked') clearPrivate();}}
  }
  function clearPrivate() {generation++; request?.abort(); listing = null; selected = null; password = ''; repeat = ''; newPassword = ''; state = state ? {...state,unlocked:false} : null; clearTimeout(timer);}
  async function lock() {
    clearPrivate(); changing = false;
    try {await api('/api/v1/album/vault/lock',{method:'POST',headers:confirm});} catch (failure) {if (!disposed) error = failure.message;}
  }
  async function submit(event) {
    event.preventDefault(); if (pending) return; error = ''; message = '';
    if ((!state.configured || changing) && (changing ? newPassword : password) !== repeat) {error = '两次输入的密码不一致'; return;}
    pending = true;
    try {
      const operation = !state.configured ? 'create' : changing ? 'password' : 'unlock';
      await api('/api/v1/album/vault/'+operation,json('POST',{password,new_password:changing ? newPassword : ''},{headers:confirm}));
      password = ''; repeat = ''; newPassword = ''; changing = false; page = 1;
      if (operation === 'create') message = '保险箱已创建，请输入独立密码解锁。';
      if (operation === 'password') {clearPrivate(); message = '密码已修改，所有保险箱会话已锁定。';}
      await refresh();
    } catch (failure) {error = failure.message; if (failure.code === 'vault_sync_pending') {changing = false; password = ''; repeat = ''; newPassword = ''; await refresh();}} finally {pending = false;}
  }
  async function move(id) {
    if (pending || readOnly) return; pending = true; error = ''; message = '';
    try {await api('/api/v1/album/vault/move',json('POST',{id},{headers:confirm,timeoutMs:55_000}));message = '照片已加密收藏，原件和普通相册缩略图已移除。'; onmoved(); await refresh();}
    catch (failure) {error = failure.message; await refresh();}
    finally {pending = false;}
  }
  function leaving() {
    clearPrivate();
    // Best effort on tab close; server expiry is authoritative if the tab vanishes.
    fetch('/api/v1/album/vault/lock',{method:'POST',headers:confirm,credentials:'same-origin',keepalive:true}).catch(() => {});
  }
  $effect(() => {if (state?.unlocked && visible && !visible()) lock();});
  onMount(() => {
    refresh(); const hidden = () => {if (document.hidden) leaving();}; const expired = () => {clearPrivate(); error = '保险箱已锁定，请重新输入密码。';};
    document.addEventListener('visibilitychange',hidden); window.addEventListener('pagehide',leaving); window.addEventListener('nasctl:vault-locked',expired);
    return () => {disposed = true; leaving(); document.removeEventListener('visibilitychange',hidden); window.removeEventListener('pagehide',leaving); window.removeEventListener('nasctl:vault-locked',expired);};
  });
</script>
<div class="photo-library photo-vault">
  <div class="photo-library-heading"><div><h1><Icon name="lock" size={24}/>照片保险箱</h1><p class="muted">只属于你的私密回忆</p></div>{#if state?.unlocked}<button class="button secondary small" onclick={lock}>锁定</button>{/if}</div>
  {#if error}<p class="notice error" role="alert">{error}</p>{/if}
  {#if message}<p class="notice success" role="status">{message}</p>{/if}
  {#if !state}<p role="status">正在读取保险箱…</p>
  {:else if !state.available}<section class="empty-state"><Icon name="lock" size={42}/><h2>保险箱暂不可用</h2><p>请先启用本地相册。</p></section>
  {:else if !state.unlocked || changing}
    <section class="vault-gate"><span class="vault-lock"><Icon name="lock" size={32}/></span><h2>{changing ? '修改保险箱密码' : state.configured ? '保险箱已锁定' : '给私密照片一个安全的家'}</h2><p class="muted">{changing ? '更改密码后，所有设备都需要重新解锁。' : state.configured ? '输入独立密码，查看你的私密照片。' : '设置独立密码。照片、缩略图和文件名均加密保存。'}</p>
      {#if readOnly && !state.configured}<p class="muted">只读模式下不能创建保险箱。</p>{:else}<form class="vault-password-form" onsubmit={submit}>
        <label>{changing ? '当前密码' : state.configured ? '保险箱密码' : '设置保险箱密码'}<input aria-label="保险箱密码" type="password" bind:value={password} required minlength={state.configured ? undefined : 12} maxlength="256" autocomplete={state.configured ? 'current-password' : 'new-password'}/></label>
        {#if changing}<label>新密码<input aria-label="保险箱新密码" type="password" bind:value={newPassword} required minlength="12" maxlength="256" autocomplete="new-password"/></label>{/if}
        {#if !state.configured || changing}<label>再次输入密码<input aria-label="再次输入保险箱密码" type="password" bind:value={repeat} required minlength="12" maxlength="256" autocomplete="new-password"/></label><p class="field-help">至少 12 个字符。请妥善保存，保险箱密码遗失后无法恢复照片。</p>{/if}
        <button class="button primary" type="submit" disabled={pending}>{pending ? '正在处理…' : changing ? '保存新密码' : state.configured ? '解锁保险箱' : '创建保险箱'}</button>
        {#if changing}<button class="text-button" type="button" onclick={() => {changing = false; password = ''; repeat = ''; newPassword = '';}}>取消</button>{/if}
      </form>{/if}
    </section>
  {:else}
    {#if candidate}<section class="vault-import"><div><strong>移入「{candidate.name}」</strong><p class="muted">校验加密副本后移除原件，不保留普通相册副本。</p></div><div class="button-group"><button class="text-button" disabled={pending} onclick={oncancel}>取消</button><button class="button primary small" disabled={pending || readOnly} onclick={() => move(candidate.id)}>{pending ? '正在加密…' : '确认加密并移入'}</button></div></section>{/if}
    <div class="vault-library-tools"><span>{listing?.total_estimate || 0} 张私密照片</span><span>15 分钟后自动锁定{#if !readOnly} · <button class="text-button" onclick={() => {changing = true; password = '';}}>修改密码</button>{/if}</span></div>
    {#if listing?.items.length}<div class="album-gallery vault-gallery">{#each listing.items as item (item.id)}<div class="vault-photo"><button class="album-tile" onclick={() => selected = item} aria-label={'查看私密照片 ' + item.name}><span class="album-frame">{#if failedThumbs[item.id]}<Icon name="album" size={32}/>{:else}<img src={item.thumb_url} alt="" loading="lazy" decoding="async" onerror={() => failedThumbs[item.id] = true}/>{/if}</span></button>{#if item.pending}<p class="notice warning">原件清理未完成</p><button class="text-button" disabled={pending || readOnly} onclick={() => move(item.source_id)}>重试完成移入</button>{/if}</div>{/each}</div>
    {:else if listing}<section class="empty-state"><Icon name="album" size={38}/><h2>留给自己的回忆</h2><p>在普通相册中打开照片，点击锁形按钮移入这里。</p><small class="muted">每张照片最多 64 MiB，需要照片目录允许写入。</small></section>{/if}
    <p class="field-help">加密照片保存在 NAS 状态盘。状态备份包不包含照片，请另行备份保险箱目录。</p>
    <footer class="photo-pager"><span>第 {page} 页</span><div class="button-group"><button class="icon-button" aria-label="保险箱上一页" disabled={pending || page <= 1} onclick={() => {page--; refresh();}}><Icon name="back" size={18}/></button><button class="icon-button" aria-label="保险箱下一页" disabled={pending || !listing?.has_next} onclick={() => {page++; refresh();}}><Icon name="forward" size={18}/></button></div></footer>
  {/if}
</div>
{#if selected && state?.unlocked}<PhotoViewer {selected} items={listing?.items || []} onselect={(item) => selected = item} onclose={() => selected = null}/>{/if}
