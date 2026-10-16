<script>
  import Icon from '../components/Icon.svelte';
  import AccountPassword from '../components/AccountPassword.svelte';
  let { auth, theme = 'system', adminPages = [], displayName = '', pending = false, onthemechange, onlogout, onpassword } = $props();
  const roleLabels = {admin:'管理员', operator:'操作者', viewer:'只读用户'};
  const role = $derived(auth?.principal === 'ai' ? '程序访问' : roleLabels[auth?.role] || (auth?.can_manage_device ? '设备管理员' : '访客'));
  const themes = [{id:'system', label:'跟随系统'}, {id:'light', label:'浅色'}, {id:'dark', label:'深色'}];
</script>
<div class="me">
  <header class="me-card panel">
    <span class="avatar large" aria-hidden="true">{(displayName || 'N').slice(0,1).toUpperCase()}</span>
    <div><h1>{displayName}</h1><p class="muted">{role}{auth?.name && auth.name !== displayName ? ' · ' + auth.name : ''}</p></div>
  </header>

  <section class="panel me-section" aria-labelledby="me-appearance">
    <div class="panel-heading"><h2 id="me-appearance">外观</h2></div>
    <div class="segmented theme-choice" role="radiogroup" aria-label="主题">{#each themes as item (item.id)}<button role="radio" aria-checked={theme === item.id} class:active={theme === item.id} onclick={() => onthemechange?.(item.id)}>{item.label}</button>{/each}</div>
  </section>

  <section class="panel me-section" aria-labelledby="me-account">
    <div class="panel-heading"><h2 id="me-account">账号</h2></div>
    <div class="me-actions">
      {#if auth?.user_id}<div class="me-row"><div><strong>登录密码</strong><p class="muted">修改后，该账号的所有会话都需要重新登录。</p></div><AccountPassword {auth} variant="button" oncomplete={onpassword}/></div>{/if}
      {#if adminPages.length}<a class="me-row" href={'#' + adminPages[0].id}><div><strong>管理后台</strong><p class="muted">{adminPages.length > 4 ? '存储、用户、备份与系统设置' : '任务、相册处理与追番订阅'}</p></div><Icon name="chevron" size={18}/></a>{/if}
      {#if auth?.required}<div class="me-row"><div><strong>退出登录</strong><p class="muted">退出后需要重新输入账号和密码。</p></div><button class="button secondary danger-text" disabled={pending} onclick={onlogout}>退出登录</button></div>{/if}
    </div>
  </section>
</div>
