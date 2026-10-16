<script>
  import ErrorNotice from './components/ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api } from './lib/api.js';
  import { modeLabels } from './lib/format.js';
  import { userPages, allowedAdminPages, resolve, pageInfo, roleOf } from './lib/routes.js';
  import { loadTheme, applyTheme } from './lib/theme.js';
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

  let theme = $state('system'), snapshot = $state(null), error = $state(''), loading = $state(false);
  let auth = $state(null), authError = $state(''), authPending = $state(false);
  let current = $state('home'), visited = $state([]), drawer = $state(false), fileSearch = $state(null), searchNumber = 0;
  let controller, disposed = false, generation = 0;

  const adminPages = $derived(auth?.authenticated ? allowedAdminPages(auth) : []);
  const inAdmin = $derived(current === 'admin' || current.startsWith('admin/'));
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
      {:else if id === 'admin/backup'}<Backup {readOnly}/>
      {:else if id === 'admin/users'}<Users {auth} onaccountschange={checkAuth}/>
      {:else if id === 'admin/system'}<System onchange={refresh}/>
      {:else if id === 'admin/logs'}<Logs/>{/if}
    </Page>
  {/each}
{/snippet}

{#snippet account()}
  <button class="theme-toggle icon-button" aria-label={theme === 'dark' || (theme === 'system' && matchMedia?.('(prefers-color-scheme: dark)')?.matches) ? '切换浅色模式' : '切换深色模式'} onclick={() => setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark')}><Icon name="moon" size={18} class="theme-moon"/><Icon name="sun" size={18} class="theme-sun"/></button>
  <AccountMenu {displayName} roleLabel={roleLabel} {theme} adminHref={adminPages[0]?.id || ''} canLogout={Boolean(auth.required)} pending={authPending} onthemechange={setTheme} onlogout={logout}/>
{/snippet}

{#if !auth || authError}
  <main class="login-page"><section class="login-card panel"><h1>连接你的 NAS</h1>{#if authError}<ErrorNotice message={authError}/><button class="button primary" disabled={authPending} onclick={checkAuth}>重新连接</button>{:else}<p role="status">正在检查访问状态…</p>{/if}</section></main>
{:else if !auth.authenticated}
  <Login available={auth.login_available} accounts={auth.accounts_enabled} setup={auth.setup_required} tokenAvailable={auth.token_login_available} onlogin={checkAuth}/>
{:else}
<a class="skip-link" href="#content">跳至主要内容</a>
<div class="shell" class:admin-shell={inAdmin} class:user-shell={!inAdmin} class:fill={pageInfo(current)?.fill}>
  {#if inAdmin}
    <aside class="admin-sidebar" class:open={drawer} aria-label="管理后台导航">
      <div class="sidebar-brand"><span class="brand-mark"><Icon name="disk" size={18}/></span><strong>nasctl</strong><span class="sidebar-badge">管理后台</span></div>
      <nav class="sidebar-nav">
        {#each groups as {group, pages: items}}
          {#if group}<p class="sidebar-group">{group}</p>{/if}
          {#each items as page (page.id)}<a class="nav-link" class:active={current === page.id} aria-current={current === page.id ? 'page' : undefined} href={'#' + page.id}><Icon name={page.icon} size={18}/><span>{page.label}</span></a>{/each}
        {/each}
      </nav>
      <a class="sidebar-exit" href="#home"><Icon name="back" size={17}/>返回我的空间</a>
    </aside>
    {#if drawer}<button class="drawer-scrim" aria-label="关闭导航" onclick={() => drawer = false}></button>{/if}
    <header class="admin-topbar">
      <button class="icon-button drawer-toggle" aria-label="打开管理导航" aria-expanded={drawer} onclick={() => drawer = !drawer}><Icon name="menu" size={20}/></button>
      <p class="topbar-title"><a href={'#' + adminPages[0]?.id}>管理后台</a><Icon name="chevron" size={14}/><strong>{pageInfo(current)?.label}</strong></p>
      <span class="system-status" title={snapshot?.mode?.reason || ''}><span class="status-dot" class:offline={!healthy}></span>{snapshot ? modeLabels[snapshot.mode?.mode] || '状态未知' : '正在连接'}</span>
      {@render account()}
    </header>
  {:else}
    <header class="app-header">
      <a class="brand" href="#home" aria-label="nasctl 首页"><span class="brand-mark"><Icon name="disk" size={18}/></span><strong>nasctl</strong></a>
      <nav class="primary-nav" aria-label="主导航">{#each userPages.filter((page) => !page.mobileOnly) as page (page.id)}<a class="nav-link" class:active={current === page.id} aria-current={current === page.id ? 'page' : undefined} href={'#' + page.id}><Icon name={page.icon} size={17}/><span>{page.label}</span></a>{/each}</nav>
      <div class="header-actions">
        {#if adminPages.length}<a class="admin-entry" href={'#' + adminPages[0].id}><Icon name="settings" size={16}/><span>管理后台</span></a>{/if}
        {@render account()}
      </div>
    </header>
  {/if}
  <main id="content" class="shell-main" tabindex="-1">{@render pages(visited)}</main>
  {#if !inAdmin}<nav class="tab-bar" aria-label="快捷导航">{#each userPages as page (page.id)}<a class:active={current === page.id} aria-current={current === page.id ? 'page' : undefined} href={'#' + page.id}><Icon name={page.icon} size={22}/><span>{page.label}</span></a>{/each}</nav>{/if}
</div>
{/if}
