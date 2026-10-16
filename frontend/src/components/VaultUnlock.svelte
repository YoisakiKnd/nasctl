<script>
  import { vault, unlockVault, openVault } from '../lib/vault.svelte.js';
  import Icon from './Icon.svelte';
  // A compact unlock form for places that meet a private directory outside the
  // vault page: the file browser and the album directory editor.
  // nested renders without its own <form>, for use inside another form.
  let { message = '这是私密目录，输入保险箱密码后才能查看。', nested = false, onunlocked } = $props();
  let password = $state(''), error = $state('');
  async function submit(event) {
    event?.preventDefault(); event?.stopPropagation(); error = '';
    if (!password || vault.pending) return;
    try {await unlockVault(password); password = ''; onunlocked?.();}
    catch (failure) {error = failure.summary || failure.message;}
  }
</script>
<div class="vault-unlock">
  <span class="vault-unlock-icon" aria-hidden="true"><Icon name="lock" size={20}/></span>
  <div class="vault-unlock-body">
    <p>{message}</p>
    {#if vault.status && !vault.status.configured}
      <p class="field-help">还没有设置保险箱密码。<button type="button" class="text-button" onclick={openVault}>去创建保险箱密码</button></p>
    {:else if vault.status && !vault.status.available}
      <p class="field-help">私密目录只对设备管理员开放。</p>
    {:else}
      {#if nested}
        <div class="vault-unlock-form">
          <input type="password" aria-label="输入保险箱密码解锁" placeholder="保险箱密码" autocomplete="current-password" maxlength="256" bind:value={password} onkeydown={(event) => {if (event.key === 'Enter') submit(event);}}/>
          <button class="button primary small" type="button" disabled={vault.pending || !password} onclick={submit}>{vault.pending ? '正在解锁…' : '解锁'}</button>
        </div>
      {:else}
        <form class="vault-unlock-form" onsubmit={submit}>
          <input type="password" aria-label="输入保险箱密码解锁" placeholder="保险箱密码" autocomplete="current-password" maxlength="256" required bind:value={password}/>
          <button class="button primary small" type="submit" disabled={vault.pending || !password}>{vault.pending ? '正在解锁…' : '解锁'}</button>
        </form>
      {/if}
      {#if error}<p class="inline-error" role="alert">{error}</p>{/if}
    {/if}
  </div>
</div>
