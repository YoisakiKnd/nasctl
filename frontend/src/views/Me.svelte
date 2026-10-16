<script>
  import Icon from '../components/Icon.svelte';
  import AccountPassword from '../components/AccountPassword.svelte';
  import { vault, lockVault, openVault, remaining } from '../lib/vault.svelte.js';
  let { auth, theme = 'system', adminPages = [], displayName = '', pending = false, onthemechange, onlogout, onpassword } = $props();
  const roleLabels = {admin:'管理员', operator:'操作者', viewer:'只读用户'};
  const role = $derived(auth?.principal === 'ai' ? '程序访问' : roleLabels[auth?.role] || (auth?.can_manage_device ? '设备管理员' : '访客'));
  const themes = [{id:'system', label:'跟随系统'}, {id:'light', label:'浅色'}, {id:'dark', label:'深色'}];
  const status = $derived(vault.status);
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
      {#if adminPages.length}<a class="me-row" href={'#' + adminPages[0].id}><span class="me-row-icon"><Icon name="settings" size={18}/></span><div><strong>管理后台</strong><p class="muted">{adminPages.length > 4 ? '存储、用户、备份与系统设置' : '任务、相册处理与追番订阅'}</p></div><Icon name="chevron" size={18}/></a>{/if}
      {#if auth?.required}<div class="me-row"><span class="me-row-icon danger"><Icon name="back" size={18}/></span><div><strong>退出登录</strong><p class="muted">退出后需要重新输入账号和密码。</p></div><button class="button secondary danger-text" disabled={pending} onclick={onlogout}>退出登录</button></div>{/if}
    </div>
  </section>
</div>
