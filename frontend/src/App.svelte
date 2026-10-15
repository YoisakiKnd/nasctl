<script>
  import { onMount } from 'svelte';
  import { api } from './lib/api.js';
  import { bytes, modeLabels } from './lib/format.js';
  import { fitWindow, newWindow } from './lib/windows.js';
  import Icon from './components/Icon.svelte';
  import AppWindow from './components/AppWindow.svelte';
  import DesktopLandscape from './components/DesktopLandscape.svelte';
  import Anime from './views/Anime.svelte';
  import Album from './views/Album.svelte';
  import Backup from './views/Backup.svelte';
  import Overview from './views/Overview.svelte';
  import Files from './views/Files.svelte';
  import Jobs from './views/Jobs.svelte';
  import Settings from './views/Settings.svelte';
  import Login from './components/Login.svelte';
  import AccountPassword from './components/AccountPassword.svelte';

  const navigation = [
    {id:'overview',label:'概览',icon:'overview',hint:'设备与存储',color:'teal'},
    {id:'files',label:'文件管理',icon:'folder',hint:'浏览与整理',color:'blue'},
    {id:'album',label:'相册',icon:'album',hint:'照片与视频',color:'rose'},
    {id:'media',label:'追番',icon:'anime',hint:'订阅与媒体库',color:'violet'},
    {id:'backup',label:'备份',icon:'backup',hint:'文件副本',color:'amber'},
    {id:'jobs',label:'任务中心',icon:'jobs',hint:'进度与记录',color:'teal'},
    {id:'settings',label:'设置',icon:'settings',hint:'设备偏好',color:'slate'},
  ];
  let theme = $state('light'), snapshot = $state(null), error = $state(''), loading = $state(false);
  let auth = $state(null), authError = $state(''), authPending = $state(false);
  let windows = $state([]), active = $state(''), layer = 10;
  let areaWidth = $state(1200), areaHeight = $state(680), mobile = $state(false), now = $state(new Date());
  let appQuery=$state(''), fileSearch=$state(null), searchNumber=0;
  const visibleApps=$derived(navigation.filter(item=>auth?.role==='viewer'?['overview','files','album','media'].includes(item.id):auth?.role==='operator'?['overview','files','album','media','jobs'].includes(item.id):true));
  const shownApps=$derived(visibleApps.filter((item)=>(item.label+item.hint+item.id).toLowerCase().includes(appQuery.trim().toLowerCase())));
  const searchRoot=$derived(roots.find((item)=>item.available));
  const memoryPercent=$derived(snapshot?.resources?.mem_total_kb>0?Math.round(100*(1-snapshot.resources.mem_available_kb/snapshot.resources.mem_total_kb)):null);
  const roots = $derived(snapshot?.files?.roots || []);
  const readOnly = $derived(!snapshot || snapshot.read_only || auth?.principal === 'ai'||auth?.read_only_user);
  let controller, disposed = false, generation = 0;
  function resize() {
    mobile = window.innerWidth <= 720;
    areaWidth = window.innerWidth;
    areaHeight = Math.max(200,window.innerHeight-144);
    windows = windows.map((frame)=>fitWindow(frame,areaWidth,areaHeight));
  }
  function remember(id) {history.replaceState(null,'',window.location.pathname+window.location.search+(id?'#'+id:''));}
  function focus(id) {
    if (active===id && !windows.find((frame)=>frame.id===id)?.minimized) return;
    active=id; windows=windows.map((frame)=>frame.id===id?{...frame,minimized:false,z:++layer}:frame); remember(id);
  }
  function open(id) {
    if (!auth?.authenticated || !visibleApps.some((item)=>item.id===id)) return;
    if (!windows.some((frame)=>frame.id===id)) windows=[...windows,newWindow(id,windows.length%5,areaWidth,areaHeight,++layer)];
    focus(id);
  }
  function nextActive() { active=[...windows].filter((frame)=>!frame.minimized).sort((a,b)=>b.z-a.z)[0]?.id || ''; remember(active); }
  function change(id,next) {
    windows=windows.map((frame)=>frame.id===id?{...next,z:frame.z}:frame);
    if(next.minimized && active===id) nextActive();
  }
  function close(id) { windows=windows.filter((frame)=>frame.id!==id); if(active===id) nextActive(); }
  function desktop() { windows=windows.map((frame)=>({...frame,minimized:true})); active=''; remember(''); }
  function searchFiles(){if(!appQuery.trim() || !searchRoot)return;fileSearch={id:++searchNumber,q:appQuery.trim().slice(0,128),root:searchRoot.id};open('files');}
  function searchApps(event){event.preventDefault();if(shownApps.length===1)open(shownApps[0].id);else if(!shownApps.length)searchFiles();}
  function route() { const hash=window.location.hash.slice(1); if(navigation.some((item)=>item.id===hash)) open(hash); }
  function toggleTheme() {
    theme = theme === 'light' ? 'dark' : 'light'; document.documentElement.dataset.theme = theme;
    try {localStorage.setItem('nasctl-theme',theme);} catch { /* storage can be disabled */ }
  }
  async function refresh() {
    if (!auth?.authenticated) return;
    const current = ++generation; controller?.abort(); controller = new AbortController(); loading = true;
    try { const data = await api('/api/v1/overview',{signal:controller.signal}); if (!disposed && current === generation) {snapshot = data; error = '';} }
    catch (failure) {if (!disposed && current === generation && failure.name !== 'AbortError') error = failure.message;}
    finally {if (!disposed && current === generation) loading = false;}
  }
  function expired() { auth = {...auth, required:true, authenticated:false}; windows=[]; active=''; appQuery=''; fileSearch=null; snapshot = null; error = ''; controller?.abort(); ++generation; }
  async function checkAuth() {
    if (authPending || disposed) return;
    authPending = true; authError = '';
    try {
      const result = await api('/api/v1/auth/status'); if (disposed) return;
      auth = result;
      if (result.authenticated) { await refresh(); route(); } else {windows=[];active='';snapshot=null;appQuery='';fileSearch=null;controller?.abort();++generation;}
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
    try {theme = localStorage.getItem('nasctl-theme') === 'dark' ? 'dark' : 'light';} catch {theme = 'light';}
    document.documentElement.dataset.theme = theme;
    resize(); checkAuth();
    window.addEventListener('hashchange',route); window.addEventListener('nasctl:unauthorized',expired);
    const poll = () => {if (!document.hidden) {now=new Date(); refresh();}};
    const interval = setInterval(poll,15000); document.addEventListener('visibilitychange',poll);
    return () => {disposed = true; controller?.abort(); clearInterval(interval); window.removeEventListener('hashchange',route); window.removeEventListener('nasctl:unauthorized',expired); document.removeEventListener('visibilitychange',poll);};
  });
</script>
<svelte:window onresize={resize}/>
{#if !auth || authError}
  <main class="login-page"><section class="login-card panel"><h1>连接你的 NAS</h1>{#if authError}<p class="notice error" role="alert">{authError}</p><button class="button primary" disabled={authPending} onclick={checkAuth}>重新连接</button>{:else}<p role="status">正在检查访问状态…</p>{/if}</section></main>
{:else if !auth.authenticated}
  <Login available={auth.login_available} accounts={auth.accounts_enabled} setup={auth.setup_required} tokenAvailable={auth.token_login_available} onlogin={checkAuth}/>
{:else}
<a class="skip-link" href="#content">跳至桌面</a>
<div class="desktop-shell">
  <DesktopLandscape/>
  <header class="topbar desktop-topbar"><button class="desktop-brand" aria-label="显示桌面" onclick={desktop}><span class="brand-symbol"><Icon name="disk" size={22}/></span><strong>nasctl</strong><span>我的空间</span></button><div class="topbar-actions"><span class="system-status"><span class="status-dot" class:offline={!snapshot || !['normal','maintenance'].includes(snapshot?.mode?.mode)}></span>{snapshot ? modeLabels[snapshot.mode?.mode] || '状态未知' : '正在连接'}</span><span class="topbar-divider"></span><button class="icon-button" aria-label={theme === 'light' ? '切换深色模式' : '切换浅色模式'} onclick={toggleTheme}><Icon name={theme === 'light' ? 'moon' : 'sun'} size={18}/></button>{#if auth.required}<button class="text-button" disabled={authPending} onclick={logout}>退出登录</button>{/if}{#if auth.user_id}<AccountPassword {auth} oncomplete={checkAuth}/>{:else}<span class="avatar" aria-label="本地操作者">N</span>{/if}</div></header>
  <main id="content" class="desktop-workspace" tabindex="-1" aria-label="NAS 桌面">
    <div class="desktop-home" inert={mobile && !!active}>
      <aside class="desktop-status-column" aria-label="设备状态">
        <section class="desktop-glass clock-widget"><time datetime={now.toISOString()}>{now.toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit',hour12:false})}</time><span>{now.toLocaleDateString('zh-CN',{month:'long',day:'numeric',weekday:'long'})}</span></section>
        <button class="desktop-glass system-widget" onclick={()=>open('overview')}><div class="desktop-widget-title"><Icon name="overview" size={16}/><strong>系统状态</strong><Icon name="chevron" size={14}/></div><div class="memory-widget"><span class="memory-ring" class:unknown={memoryPercent===null} style:--memory={(memoryPercent ?? 0)+'%'}><span>{memoryPercent===null?'—':memoryPercent+'%'}<small>内存已用</small></span></span><div><strong>{snapshot?.resources?bytes(snapshot.resources.mem_available_kb*1024):'未获取'}</strong><small>可用内存</small><span>{snapshot?.resources?'共 '+bytes(snapshot.resources.mem_total_kb*1024):'资源数据暂不可用'}</span></div></div><div class="desktop-guard"><span class="status-dot" class:offline={!snapshot?.resources}></span>{snapshot?.resources?.verdict ? snapshot.resources.verdict==='allow'?'资源保护 · 允许处理':'资源保护 · '+(snapshot.resources.reason||'限制处理'):'资源保护状态未获取'}</div></button>
        <section class="desktop-glass storage-widget"><div class="desktop-widget-title"><Icon name="disk" size={16}/><strong>存储空间</strong><button class="icon-button" aria-label="查看存储目录" onclick={()=>open('files')}><Icon name="chevron" size={14}/></button></div>{#if roots.length}{#each roots.slice(0,4) as root}<button class="desktop-storage" onclick={()=>open('files')}><span class="desktop-storage-icon"><Icon name="disk" size={21}/></span><div><strong>{root.label || root.id}</strong><small>{!root.available?'暂不可用':root.capacity?bytes(root.capacity.available_bytes)+' 可用 / '+bytes(root.capacity.total_bytes):'容量暂不可用'}</small>{#if root.capacity}<progress class="capacity-meter desktop-capacity-meter" value={root.capacity.used_bytes} max={root.capacity.total_bytes} aria-label={(root.label||root.id)+' 文件系统空间使用率'}></progress>{/if}</div><span class="status-dot" class:offline={!root.available}></span></button>{/each}{:else}<p class="desktop-widget-empty">{snapshot?'尚未添加存储目录':'正在读取…'}</p>{/if}</section>
        {#if auth.can_manage_device!==false}<button class="desktop-glass desktop-settings-link" onclick={()=>open('settings')}><Icon name="settings" size={17}/>设备设置<Icon name="chevron" size={14}/></button>{/if}
      </aside>
      <section class="desktop-app-column">
        <form class="desktop-search desktop-glass" onsubmit={searchApps}><Icon name="search" size={18}/><input aria-label="搜索应用或文件" placeholder="搜索应用或文件…" bind:value={appQuery} maxlength="128"/><kbd>↵</kbd></form>
        {#if appQuery.trim() && searchRoot}<button class="desktop-file-search desktop-glass" onclick={searchFiles}><Icon name="folder" size={18}/><span>在“{searchRoot.label || searchRoot.id}”中搜索 <strong>{appQuery}</strong></span><Icon name="chevron" size={16}/></button>{/if}
        {#if auth.setup_requires_config&&auth.can_manage_device!==false}<div class="notice desktop-onboarding"><div><strong>启用账号登录</strong><p>先在本机配置持久登录保护，再为成员创建账号。</p></div><button class="button primary small" onclick={()=>open('settings')}>查看设置步骤</button></div>{:else if auth.setup_available&&!auth.accounts_enabled&&auth.can_manage_device!==false}<div class="notice desktop-onboarding"><div><strong>设置管理员账号</strong><p>使用用户名和密码登录你的 NAS，并为成员分配角色。</p></div><button class="button primary small" onclick={()=>open('settings')}>开始设置</button></div>{:else if snapshot&&roots.length===0&&auth.can_manage_device!==false}<div class="notice desktop-onboarding"><div><strong>添加你的存储目录</strong><p>选择 NAS 上已准备好的目录，开始浏览和管理文件。</p></div><button class="button primary small" onclick={()=>open('settings')}>添加目录</button></div>{/if}<div class="desktop-section-heading"><h1>应用</h1><span>你的私人空间</span></div>
        <nav class="desktop-apps" aria-label="应用">{#each shownApps as item}<button class={'desktop-app nav-link '+item.color} onclick={()=>open(item.id)}><span class="app-icon"><Icon name={item.icon} size={29}/></span><strong>{item.label}</strong><small>{item.hint}</small></button>{/each}</nav>
        {#if !shownApps.length}<p class="desktop-no-results">没有匹配的应用{searchRoot?'，可在上方搜索文件。':'。'}</p>{/if}
        {#if visibleApps.some(item=>item.id==='jobs')}<button class="desktop-glass desktop-task-summary" onclick={()=>open('jobs')}><span class="task-summary-icon"><Icon name="jobs" size={23}/></span><div><strong>任务动态</strong><span>{snapshot ? snapshot.running?.length ? snapshot.running.length+' 项任务正在进行' : '此刻没有任务运行' : '正在读取任务…'}</span></div>{#if snapshot?.attention?.length}<span class="desktop-attention">{snapshot.attention.length} 项需要留意</span>{:else}<span class="desktop-task-idle">{snapshot?'一切有序':'未获取'}</span>{/if}<Icon name="chevron" size={17}/></button>{/if}
        {#if error}<p class="notice error" role="alert">设备状态读取失败：{error}</p>{/if}
        <p class="desktop-tip">点击应用打开窗口 · 双击标题栏最大化 · 底部随时切换</p>
      </section>
    </div>
    {#each windows as frame (frame.id)}{@const app=navigation.find((item)=>item.id===frame.id)}
      <AppWindow {frame} {app} active={active===frame.id} {mobile} {areaWidth} {areaHeight} onfocus={()=>focus(frame.id)} onchange={(next)=>change(frame.id,next)} onclose={()=>close(frame.id)}>
        {#if frame.id === 'media'}<Anime {readOnly} viewer={auth.role==='viewer'}/>
        {:else if frame.id === 'overview'}<Overview {snapshot} {error} {loading} jobsAvailable={visibleApps.some(item=>item.id==='jobs')} onrefresh={refresh}/>
        {:else if frame.id === 'files'}<Files {readOnly} agent={auth.principal === 'ai'} searchRequest={fileSearch}/>
        {:else if frame.id === 'jobs'}<Jobs/>
        {:else if frame.id === 'settings'}<Settings onchange={refresh} {auth} onaccountschange={checkAuth}/>
        {:else if frame.id === 'album'}<Album {readOnly} canConfigure={auth.can_manage_device!==false}/>
        {:else if frame.id === 'backup'}<Backup {readOnly}/>{/if}
      </AppWindow>
    {/each}
  </main>
  <footer class="desktop-dock-area"><nav class="desktop-dock" aria-label="窗口切换"><button class="dock-item" class:active={!active} aria-label="显示桌面" onclick={desktop}><Icon name="home" size={23}/><span>桌面</span></button>{#if windows.length}<span class="dock-divider"></span>{/if}{#each windows as frame (frame.id)}{@const app=navigation.find((item)=>item.id===frame.id)}<button class={'dock-item '+app.color} class:active={active===frame.id} class:minimized={frame.minimized} aria-label={'切换到'+app.label+(frame.minimized?'，已最小化':'')} aria-pressed={active===frame.id} onclick={()=>focus(frame.id)}><Icon name={app.icon} size={23}/><span>{app.label}</span><i></i></button>{/each}</nav><span class="desktop-version">私人存储 · nasctl</span></footer>
</div>
{/if}
