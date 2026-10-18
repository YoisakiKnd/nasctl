<script>
  import Icon from '../components/Icon.svelte';
  import AccountPassword from '../components/AccountPassword.svelte';
  import { vault, lockVault, openVault, remaining } from '../lib/vault.svelte.js';
  import { desktop, wallpapers, saveDesktop, uploadWallpaper, removeWallpaper } from '../lib/desktop.svelte.js';
  import ErrorNotice from '../components/ErrorNotice.svelte';
  let { auth, theme = 'system', adminPages = [], displayName = '', pending = false, onthemechange, onlogout, onpassword } = $props();
  const roleLabels = {admin:'管理员', operator:'操作者', viewer:'只读用户'};
  const role = $derived(auth?.principal === 'ai' ? '程序访问' : roleLabels[auth?.role] || (auth?.can_manage_device ? '设备管理员' : '访客'));
  const themes = [{id:'system', label:'跟随系统'}, {id:'light', label:'浅色'}, {id:'dark', label:'深色'}];
  const status = $derived(vault.status);
  const manager = $derived(auth?.can_manage_device !== false && auth?.principal !== 'ai');
  let uploading = $state(false), wallpaperError = $state(''), picker = $state();
  function choose(id) {desktop.wallpaper = id; saveDesktop();}
  async function upload(event) {
    const file = event.currentTarget.files?.[0]; event.currentTarget.value = '';
    if (!file) return;
    uploading = true; wallpaperError = '';
    try {await uploadWallpaper(file);} catch (failure) {wallpaperError = failure.message;} finally {uploading = false;}
  }
  async function remove() {
    uploading = true; wallpaperError = '';
    try {await removeWallpaper();} catch (failure) {wallpaperError = failure.message;} finally {uploading = false;}
  }
</script>
<div class="me">
  <header class="me-card panel">
    <span class="avatar large" aria-hidden="true">{(displayName || 'N').slice(0,1).toUpperCase()}</span>
    <div class="me-identity"><h1>{displayName}</h1><p><span class="badge">{role}</span>{#if auth?.name && auth.name !== displayName}<span class="muted">{auth.name}</span>{/if}</p></div>
  </header>

  <section class="panel me-section" aria-labelledby="me-appearance">
    <h2 id="me-appearance" class="me-section-title">外观</h2>
    <div class="theme-choice" role="radiogroup" aria-label="主题">{#each themes as item (item.id)}<button role="radio" aria-checked={theme === item.id} class:active={theme === item.id} onclick={() => onthemechange?.(item.id)}><span class="theme-swatch {item.id}" aria-hidden="true"></span>{item.label}</button>{/each}</div>
  </section>

  <section class="panel me-section" aria-labelledby="me-wallpaper">
    <h2 id="me-wallpaper" class="me-section-title">壁纸</h2>
    <div class="wallpaper-choice" role="radiogroup" aria-label="壁纸">
      {#each wallpapers as item (item.id)}<button role="radio" aria-checked={desktop.wallpaper === item.id} class:active={desktop.wallpaper === item.id} onclick={() => choose(item.id)}><span class="wallpaper-swatch" style:background={item.css}></span>{item.label}</button>{/each}
      {#if desktop.custom}<button role="radio" aria-checked={desktop.wallpaper === 'custom'} class:active={desktop.wallpaper === 'custom'} onclick={() => choose('custom')}><span class="wallpaper-swatch" style:background={`center / cover url("/api/v1/desktop/wallpaper?v=${desktop.revision}")`}></span>自定义</button>{/if}
    </div>
    {#if manager}<div class="wallpaper-actions"><button class="button secondary small" disabled={uploading} onclick={() => picker.click()}><Icon name="upload" size={15}/>{uploading ? '正在上传…' : desktop.custom ? '更换自定义壁纸' : '上传自定义壁纸'}</button>{#if desktop.custom}<button class="text-button danger-text" disabled={uploading} onclick={remove}>删除自定义壁纸</button>{/if}<input bind:this={picker} type="file" accept="image/jpeg,image/png,image/webp" hidden onchange={upload}/></div><p class="field-help">自定义壁纸对 NAS 的所有成员可用，大图会在上传前自动缩小。选择哪张壁纸只影响这台设备。</p>{/if}
    {#if wallpaperError}<ErrorNotice message={wallpaperError}/>{/if}
  </section>

  {#if status?.available}
    <section class="panel me-section" aria-labelledby="me-privacy">
      <h2 id="me-privacy" class="me-section-title">隐私</h2>
      <div class="me-actions">
        <div class="me-row"><span class="me-row-icon private"><Icon name={status.unlocked ? 'unlock' : 'lock'} size={18}/></span><div><strong>保险箱</strong><p class="muted">{!status.configured ? '还没有设置保险箱密码' : status.unlocked ? `已解锁，闲置 ${remaining(status)}后自动锁定` : '已锁定'}{status.private_folders ? ` · ${status.private_folders} 个私密目录` : ''}</p></div>{#if status.unlocked}<button class="button secondary small" onclick={lockVault}>立即锁定</button>{:else}<button class="button secondary small" onclick={openVault}>{status.configured ? '打开保险箱' : '去设置'}</button>{/if}</div>
      </div>
    </section>
  {/if}

  <section class="panel me-section" aria-labelledby="me-account">
    <h2 id="me-account" class="me-section-title">账号</h2>
    <div class="me-actions">
      {#if auth?.user_id}<div class="me-row"><span class="me-row-icon"><Icon name="lock" size={18}/></span><div><strong>登录密码</strong><p class="muted">修改后，该账号的所有会话都需要重新登录。</p></div><AccountPassword {auth} variant="button" oncomplete={onpassword}/></div>{/if}
      {#if adminPages.length}<a class="me-row" href={'#' + adminPages[0].id}><span class="me-row-icon"><Icon name="settings" size={18}/></span><div><strong>管理设置</strong><p class="muted">{adminPages.length > 4 ? '存储、用户、备份与系统设置' : '任务、相册处理与追番订阅'}</p></div><Icon name="chevron" size={18}/></a>{/if}
      {#if auth?.required}<div class="me-row"><span class="me-row-icon danger"><Icon name="back" size={18}/></span><div><strong>退出登录</strong><p class="muted">退出后需要重新输入账号和密码。</p></div><button class="button secondary danger-text" disabled={pending} onclick={onlogout}>退出登录</button></div>{/if}
    </div>
  </section>
</div>
