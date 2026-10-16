<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api, json, query } from '../lib/api.js';
  import { vault, refreshVault, createVault, unlockVault, changeVaultPassword, lockVault, vaultLockedResponse, remaining } from '../lib/vault.svelte.js';
  import Icon from './Icon.svelte';
  import PhotoViewer from './PhotoViewer.svelte';
  import PrivateLibrary from './PrivateLibrary.svelte';
  let { readOnly = true, candidate = null, canManage = false, oncancel, onmoved } = $props();
  let listing = $state(null), selected = $state(null), page = $state(1), password = $state(''), repeat = $state(''), newPassword = $state(''), changing = $state(false), pending = $state(false), error = $state(''), message = $state(''), failedThumbs = $state({});
  let tab = $state(''), now = $state(Date.now());
  let disposed = false, request, generation = 0;
  const confirm = {'X-Nasctl-Confirm':'1'};
  const status = $derived(vault.status);
  const unlocked = $derived(Boolean(status?.unlocked));
  const folders = $derived(status?.private_folders || 0);
  // Encrypted photos are the older way to keep a photo private; the tab shows
  // only when there is something in it or a photo is waiting to be moved in.
  const showEncrypted = $derived(Boolean(candidate) || (listing?.total_estimate || 0) > 0 || !folders);
  const current = $derived(candidate ? 'encrypted' : tab || (folders ? 'folders' : 'encrypted'));
  async function loadEncrypted() {
    const mine = ++generation; request?.abort(); request = new AbortController();
    try {
      const result = await api(query('/api/v1/album/vault/media',{page}),{signal:request.signal});
      if (!disposed && generation === mine) listing = {...result,items:result.items.map((item) => ({...item,vault:true}))};
    } catch (failure) {if (!disposed && mine === generation && failure.name !== 'AbortError' && !vaultLockedResponse(failure)) error = failure.message;}
  }
  function clearPrivate() {generation++; request?.abort(); listing = null; selected = null; password = ''; repeat = ''; newPassword = ''; changing = false;}
  async function submit(event) {
    event.preventDefault(); if (pending) return; error = ''; message = '';
    if ((!status.configured || changing) && (changing ? newPassword : password) !== repeat) {error = '两次输入的密码不一致'; return;}
    pending = true;
    try {
      if (!status.configured) {await createVault(password); message = '保险箱已创建，请输入密码解锁。';}
      else if (changing) {await changeVaultPassword(password, newPassword); await lockVault(); message = '密码已修改，所有设备上的保险箱都已锁定。';}
      else await unlockVault(password);
      password = ''; repeat = ''; newPassword = ''; changing = false; page = 1;
    } catch (failure) {
      error = failure.message;
      if (failure.code === 'vault_sync_pending') {changing = false; password = ''; repeat = ''; newPassword = ''; await refreshVault();}
    } finally {pending = false;}
  }
  async function move(id) {
    if (pending || readOnly) return; pending = true; error = ''; message = '';
    try {await api('/api/v1/album/vault/move',json('POST',{id},{headers:confirm,timeoutMs:55_000})); message = '照片已加密收藏，原件和普通相册缩略图已移除。'; onmoved(); await loadEncrypted();}
    catch (failure) {if (!vaultLockedResponse(failure)) error = failure.message; await loadEncrypted();}
    finally {pending = false;}
  }
  $effect(() => {if (unlocked) loadEncrypted(); else clearPrivate();});
  onMount(() => {
    refreshVault();
    const tick = setInterval(() => now = Date.now(), 15_000);
    const locked = () => {clearPrivate(); if (!disposed) message = '';};
    window.addEventListener('nasctl:vault-locked', locked);
    return () => {disposed = true; request?.abort(); clearInterval(tick); window.removeEventListener('nasctl:vault-locked', locked);};
  });
</script>
<div class="photo-library photo-vault">
  {#if !status}<p class="loading-state" role="status">正在读取保险箱…</p>
  {:else if !status.available}<section class="empty-state"><Icon name="lock" size={42}/><h2>保险箱暂不可用</h2><p>保险箱需要启用本地相册，并且只对设备管理员开放。</p></section>
  {:else if !unlocked || changing}
    <section class="vault-gate panel">
      <span class="vault-lock" aria-hidden="true"><Icon name="lock" size={30}/></span>
      <h2>{changing ? '修改保险箱密码' : status.configured ? '保险箱已锁定' : '给私密照片一个安全的家'}</h2>
      <p class="muted">{changing ? '修改后，所有设备上的保险箱都需要用新密码重新解锁。' : status.configured ? (folders ? `输入密码后，可以查看 ${folders} 个私密目录里的照片和视频。` : '输入保险箱密码，查看你的私密照片。') : '设置一个独立于登录密码的保险箱密码。之后可以把相册目录设为私密：文件留在原处，只有输入这个密码才能看到。'}</p>
      {#if error}<ErrorNotice message={error}/>{/if}
      {#if message}<p class="notice success" role="status">{message}</p>{/if}
      {#if readOnly && !status.configured}<p class="muted">只读模式下不能创建保险箱。</p>{:else}<form class="vault-password-form" onsubmit={submit}>
        <label>{changing ? '当前密码' : status.configured ? '保险箱密码' : '设置保险箱密码'}<input aria-label="保险箱密码" type="password" bind:value={password} required minlength={status.configured ? undefined : 12} maxlength="256" autocomplete={status.configured ? 'current-password' : 'new-password'}/></label>
        {#if changing}<label>新密码<input aria-label="保险箱新密码" type="password" bind:value={newPassword} required minlength="12" maxlength="256" autocomplete="new-password"/></label>{/if}
        {#if !status.configured || changing}<label>再次输入密码<input aria-label="再次输入保险箱密码" type="password" bind:value={repeat} required minlength="12" maxlength="256" autocomplete="new-password"/></label><p class="field-help">至少 12 个字符。请妥善保存：忘记密码后，加密照片无法恢复；私密目录需要通过 SSH 取消私密设置。</p>{/if}
        <button class="button primary" type="submit" disabled={pending || vault.pending}>{pending ? '正在处理…' : changing ? '保存新密码' : status.configured ? '解锁保险箱' : '创建保险箱'}</button>
        {#if changing}<button class="text-button" type="button" onclick={() => {changing = false; password = ''; repeat = ''; newPassword = '';}}>取消</button>{/if}
      </form>{/if}
      {#if status.configured && !changing}<ul class="vault-facts"><li><Icon name="folder" size={15}/>私密目录的文件留在原位置，不占用系统盘</li><li><Icon name="clock" size={15}/>闲置 15 分钟或离开页面后自动锁定</li><li><Icon name="user" size={15}/>仅设备管理员可解锁</li></ul>{/if}
    </section>
  {:else}
    <div class="vault-bar">
      <span class="vault-state"><span class="status-dot"></span>已解锁 · 闲置 {remaining(status, now)}后自动锁定</span>
      <div class="button-group">{#if !readOnly}<button class="button secondary small" onclick={() => {changing = true; password = ''; error = ''; message = '';}}>修改密码</button>{/if}<button class="button secondary small" onclick={lockVault}><Icon name="lock" size={15}/>锁定</button></div>
    </div>
    {#if error}<ErrorNotice message={error}/>{/if}
    {#if message}<p class="notice success" role="status">{message}</p>{/if}
    {#if showEncrypted}<div class="segmented vault-tabs" role="tablist" aria-label="保险箱内容">
      <button role="tab" aria-selected={current === 'folders'} class:active={current === 'folders'} disabled={Boolean(candidate)} onclick={() => tab = 'folders'}>私密目录{#if folders}<span>{folders}</span>{/if}</button>
      <button role="tab" aria-selected={current === 'encrypted'} class:active={current === 'encrypted'} onclick={() => tab = 'encrypted'}>加密照片{#if listing?.total_estimate}<span>{listing.total_estimate}</span>{/if}</button>
    </div>{/if}
    {#if current === 'folders'}
      <PrivateLibrary {canManage} {readOnly} {folders}/>
    {:else}
      {#if candidate}<section class="vault-import"><div><strong>移入「{candidate.name}」</strong><p class="muted">校验加密副本后移除原件，不保留普通相册副本。</p></div><div class="button-group"><button class="text-button" disabled={pending} onclick={oncancel}>取消</button><button class="button primary small" disabled={pending || readOnly} onclick={() => move(candidate.id)}>{pending ? '正在加密…' : '确认加密并移入'}</button></div></section>{/if}
      {#if listing?.items.length}<div class="album-gallery vault-gallery">{#each listing.items as item (item.id)}<div class="vault-photo"><button class="album-tile" onclick={() => selected = item} aria-label={'查看私密照片 ' + item.name}><span class="album-frame">{#if failedThumbs[item.id]}<Icon name="album" size={32}/>{:else}<img src={item.thumb_url} alt="" loading="lazy" decoding="async" onerror={() => failedThumbs[item.id] = true}/>{/if}</span></button>{#if item.pending}<p class="notice warning">原件清理未完成</p><button class="text-button" disabled={pending || readOnly} onclick={() => move(item.source_id)}>重试完成移入</button>{/if}</div>{/each}</div>
      {:else if listing}<section class="empty-state"><Icon name="lock" size={38}/><h2>还没有加密照片</h2><p>{folders ? '私密目录里的照片在“私密目录”中查看。' : '推荐把整个相册目录设为私密：文件留在原处，解锁后才能看到。'}</p>{#if canManage && !folders}<a class="button primary" href="#admin/album">设置私密目录</a>{/if}<small class="muted">也可以在普通相册中打开照片，点击锁形按钮把单张照片加密移入这里（加密副本保存在系统盘）。</small></section>{/if}
      {#if listing?.items.length}<p class="field-help">加密照片保存在 NAS 状态目录（通常在系统盘）。状态备份包不包含照片，请另行备份。</p>
      <footer class="photo-pager"><span>第 {page} 页</span><div class="button-group"><button class="icon-button" aria-label="保险箱上一页" disabled={pending || page <= 1} onclick={() => {page--; loadEncrypted();}}><Icon name="back" size={18}/></button><button class="icon-button" aria-label="保险箱下一页" disabled={pending || !listing?.has_next} onclick={() => {page++; loadEncrypted();}}><Icon name="forward" size={18}/></button></div></footer>{/if}
    {/if}
  {/if}
</div>
{#if selected && unlocked}<PhotoViewer {selected} items={listing?.items || []} onselect={(item) => selected = item} onclose={() => selected = null}/>{/if}
