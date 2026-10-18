<script>
  import ErrorNotice from './components/ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api } from './lib/api.js';
  import { modeLabels } from './lib/format.js';
  import { userPages, allowedAdminPages, resolve, pageInfo, roleOf } from './lib/routes.js';
  import { loadTheme, applyTheme } from './lib/theme.js';
  import { vault, startVault, lockVault } from './lib/vault.svelte.js';
  import { wallpaperCSS, checkCustomWallpaper } from './lib/desktop.svelte.js';
  import Icon from './components/Icon.svelte';
  import Page from './components/Page.svelte';
  import Login from './components/Login.svelte';
  import Home from './views/Home.svelte';
  import Files from './views/Files.svelte';
  import Album from './views/Album.svelte';
  import Anime from './views/Anime.svelte';
  import Me from './views/Me.svelte';
  import AccountMenu from './components/AccountMenu.svelte';
  import Overview from './views/Overview.svelte';
  import Jobs from './views/Jobs.svelte';
  import AlbumAdmin from './views/admin/AlbumAdmin.svelte';
  import Backup from './views/Backup.svelte';
  import Users from './views/admin/Users.svelte';
  import System from './views/admin/System.svelte';
  import Logs from './views/admin/Logs.svelte';
  import Storage from './views/admin/Storage.svelte';

  let theme = $state('system'), snapshot = $state(null), error = $state(''), loading = $state(false);
  let auth = $state(null), authError = $state(''), authPending = $state(false);
  let current = $state('home'), visited = $state([]), drawer = $state(false), fileSearch = $state(null), searchNumber = 0;
  let controller, disposed = false, generation = 0;

  const adminPages = $derived(auth?.authenticated ? allowedAdminPages(auth) : []);
  const inAdmin = $derived(current === 'admin' || current.startsWith('admin/'));
  const inSettings = $derived(inAdmin || current === 'me');
  const readOnly = $derived(!snapshot || snapshot.read_only || auth?.principal === 'ai' || auth?.read_only_user);
  const canConfigure = $derived(auth?.can_manage_device !== false);
  const role = $derived(roleOf(auth));
  const groups = $derived([...new Set(adminPages.map((page) => page.group))].map((group) => ({group, pages:adminPages.filter((page) => page.group === group)})));
  const healthy = $derived(snapshot && ['normal','maintenance'].includes(snapshot.mode?.mode));
  const roleLabel = $derived(auth?.principal === 'ai' ? '程序访问' : ({admin:'管理员', operator:'操作员', viewer:'只读用户'})[auth?.role] || (auth?.can_manage_device ? '设备管理员' : ''));
  const displayName = $derived(auth?.display_name || auth?.name || (auth?.principal === 'local' ? '本机操作者' : '我'));

  function route() {
    if (!auth?.authenticated) return;
    const next = resolve(window.location.hash, auth);
    if (window.location.hash.replace(/^#\/?/,'') !== next && !(next === 'home' && !window.location.hash.replace(/^#\/?/,''))) history.replaceState(null, '', '#' + next);
    current = next; drawer = false;
    if (!visited.includes(next)) visited = [...visited, next];
    if (next === 'home' || next === 'admin') refresh();
  }
  // Only device managers can unlock the vault, so only they track it.
  $effect(() => {if (auth?.authenticated && canConfigure && auth.principal !== 'ai') return startVault();});
  $effect(() => {if (auth?.authenticated && auth.principal !== 'ai') checkCustomWallpaper();});
  function go(id) { if (window.location.hash !== '#' + id) window.location.hash = id; else route(); }
  function searchFiles(request) { fileSearch = {...request, id:++searchNumber}; go('files'); }
  function setTheme(value) { theme = value; applyTheme(value); }

  async function refresh() {
    if (!auth?.authenticated) return;
    const mine = ++generation; controller?.abort(); controller = new AbortController(); loading = true;
    try { const data = await api('/api/v1/overview',{signal:controller.signal}); if (!disposed && mine === generation) {snapshot = data; error = '';} }
    catch (failure) {if (!disposed && mine === generation && failure.name !== 'AbortError') error = failure.message;}
    finally {if (!disposed && mine === generation) loading = false;}
  }
  function reset() { visited = []; current = 'home'; snapshot = null; error = ''; fileSearch = null; drawer = false; controller?.abort(); ++generation; }
  function expired() { auth = {...auth, required:true, authenticated:false}; reset(); }
  async function checkAuth() {
    if (authPending || disposed) return;
    authPending = true; authError = '';
    try {
      const result = await api('/api/v1/auth/status'); if (disposed) return;
      auth = result;
      if (result.authenticated) { visited = []; route(); if (current !== 'home' && current !== 'admin') refresh(); } else reset();
    } catch (failure) { if (!disposed) authError = failure.message; }
    finally { if (!disposed) authPending = false; }
  }
  async function logout() {
    if (authPending) return; authPending = true;
    try { await api('/api/v1/auth/logout',{method:'POST'}); expired(); }
    catch (failure) { error = failure.message; }
    finally { authPending = false; }
  }
  onMount(() => {
    theme = loadTheme();
    checkAuth();
    window.addEventListener('hashchange',route); window.addEventListener('nasctl:unauthorized',expired);
    // The overview feeds the home storage card and the admin dashboard only.
    const poll = () => {if (!document.hidden && (current === 'home' || current === 'admin')) refresh();};
    const interval = setInterval(poll,15000); document.addEventListener('visibilitychange',poll);
    return () => {disposed = true; controller?.abort(); clearInterval(interval); window.removeEventListener('hashchange',route); window.removeEventListener('nasctl:unauthorized',expired); document.removeEventListener('visibilitychange',poll);};
  });
</script>

{#snippet pages(list)}
  {#each list as id (id)}{@const page = pageInfo(id)}
    <Page {page} active={current === id}>
      {#if id === 'home'}<Home {auth} {snapshot} {error} {adminPages} {readOnly} {canConfigure} onsearch={searchFiles}/>
      {:else if id === 'files'}<Files {readOnly} agent={auth.principal === 'ai'} searchRequest={fileSearch}/>
      {:else if id === 'album'}<Album {readOnly} {canConfigure} canManage={adminPages.some((item) => item.id === 'admin/album')}/>
      {:else if id === 'anime'}<Anime {readOnly} viewer={role === 'viewer'} canManage={adminPages.some((item) => item.id === 'admin/anime')}/>
      {:else if id === 'me'}<Me {auth} {theme} {adminPages} {displayName} pending={authPending} onthemechange={setTheme} onlogout={logout} onpassword={checkAuth}/>
      {:else if id === 'admin'}<Overview {snapshot} {error} {loading} jobsAvailable={true} canConfigure={canConfigure && auth.principal !== 'ai'} onrefresh={refresh}/>
      {:else if id === 'admin/jobs'}<Jobs/>
      {:else if id === 'admin/anime'}<Anime {readOnly} admin/>
      {:else if id === 'admin/album'}<AlbumAdmin {readOnly} {canConfigure}/>
      {:else if id === 'admin/storage'}<Storage {readOnly}/>
      {:else if id === 'admin/backup'}<Backup {readOnly}/>
      {:else if id === 'admin/users'}<Users {auth} onaccountschange={checkAuth}/>
      {:else if id === 'admin/system'}<System onchange={refresh}/>
      {:else if id === 'admin/logs'}<Logs/>{/if}
    </Page>
  {/each}
{/snippet}

{#snippet account()}
  {#if vault.status?.unlocked}<button class="vault-pill" title="私密内容已解锁，点击立即锁定" aria-label="私密内容已解锁，点击立即锁定" onclick={lockVault}><Icon name="unlock" size={15}/><span>已解锁</span></button>{/if}
  <button class="theme-toggle icon-button" aria-label={theme === 'dark' || (theme === 'system' && matchMedia?.('(prefers-color-scheme: dark)')?.matches) ? '切换浅色模式' : '切换深色模式'} onclick={() => setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark')}><Icon name="moon" size={18} class="theme-moon"/><Icon name="sun" size={18} class="theme-sun"/></button>
  <AccountMenu {displayName} roleLabel={roleLabel} {theme} adminHref={adminPages[0]?.id || ''} canLogout={Boolean(auth.required)} pending={authPending} onthemechange={setTheme} onlogout={logout}/>
{/snippet}

{#if !auth || authError}
  <main class="login-page"><section class="login-card panel"><h1>连接你的 NAS</h1>{#if authError}<ErrorNotice message={authError}/><button class="button primary" disabled={authPending} onclick={checkAuth}>重新连接</button>{:else}<p role="status">正在检查访问状态…</p>{/if}</section></main>
{:else if !auth.authenticated}
  <Login available={auth.login_available} accounts={auth.accounts_enabled} setup={auth.setup_required} tokenAvailable={auth.token_login_available} onlogin={checkAuth}/>
{:else}
<a class="skip-link" href="#content">跳至主要内容</a>
<!-- One persistent header for every page, Nextcloud style: apps across the
     top, each app's own navigation on the left of a rounded content area.
     The admin pages and the account page form the "设置" app. -->
<div class="shell" class:settings-shell={inSettings} class:desktop-shell={current === 'home'} class:fill={pageInfo(current)?.fill} style:--wallpaper={wallpaperCSS()}>
  <header class="app-header">
    {#if inSettings}<button class="icon-button drawer-toggle" aria-label="打开设置导航" aria-expanded={drawer} onclick={() => drawer = !drawer}><Icon name="menu" size={20}/></button>{/if}
    <a class="brand" href="#home" aria-label="nasctl 首页"><span class="brand-mark"><Icon name="disk" size={18}/></span><strong>nasctl</strong></a>
    <nav class="primary-nav" aria-label="主导航">{#each userPages.filter((page) => !page.mobileOnly) as page (page.id)}<a class="nav-link" class:active={current === page.id} aria-current={current === page.id ? 'page' : undefined} href={'#' + page.id}><Icon name={page.icon} size={18}/><span>{page.label}</span></a>{/each}</nav>
    <div class="header-actions">
      {#if adminPages.length}<a class="admin-entry" class:active={inAdmin} href={'#' + adminPages[0].id} title="管理设置" aria-label="管理设置"><Icon name="settings" size={19}/><span>管理</span></a>{/if}
      {@render account()}
    </div>
  </header>
  <div class="app-container" class:with-navigation={inSettings}>
    {#if inSettings}
      <aside class="app-navigation settings-navigation" class:admin-sidebar={adminPages.length > 0} class:open={drawer} aria-label="设置导航">
        <p class="sidebar-group">个人</p>
        <a class="nav-link" class:active={current === 'me'} aria-current={current === 'me' ? 'page' : undefined} href="#me"><Icon name="user" size={18}/><span>账号与外观</span></a>
        {#if adminPages.length}
          <nav class="sidebar-nav" aria-label="管理设置">
            {#each groups as {group, pages: items}}
              <p class="sidebar-group">{group || '管理'}</p>
              {#each items as page (page.id)}<a class="nav-link" class:active={current === page.id} aria-current={current === page.id ? 'page' : undefined} href={'#' + page.id}><Icon name={page.icon} size={18}/><span>{page.label}</span></a>{/each}
            {/each}
          </nav>
        {/if}
        <div class="navigation-foot">
          {#if inAdmin}<span class="system-status" title={snapshot?.mode?.reason || ''}><span class="status-dot" class:offline={!healthy}></span>{snapshot ? modeLabels[snapshot.mode?.mode] || '状态未知' : '正在连接'}</span>{/if}
          <a class="sidebar-exit" href="#home"><Icon name="back" size={17}/>返回首页</a>
        </div>
      </aside>
      {#if drawer}<button class="drawer-scrim" aria-label="关闭导航" onclick={() => drawer = false}></button>{/if}
    {/if}
    <main id="content" class="shell-main" tabindex="-1">{@render pages(visited)}</main>
  </div>
  <nav class="tab-bar" aria-label="快捷导航">{#each userPages as page (page.id)}<a class:active={current === page.id || (page.id === 'me' && inSettings)} aria-current={current === page.id ? 'page' : undefined} href={'#' + page.id}><Icon name={page.icon} size={22}/><span>{page.label}</span></a>{/each}</nav>
</div>
{/if}
