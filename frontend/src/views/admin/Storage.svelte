<script>
  import ErrorNotice from '../../components/ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api, json } from '../../lib/api.js';
  import { bytes } from '../../lib/format.js';
  import Icon from '../../components/Icon.svelte';
  import Dialog from '../../components/Dialog.svelte';
  // Disks, Samba shares and network storage. Everything here goes through the
  // root storage helper; the console itself cannot touch devices.
  let { readOnly = true } = $props();
  let view = $state(null), loading = $state(true), error = $state(''), message = $state(''), tab = $state('disks'), pending = $state(false), dialogError = $state('');
  let smart = $state({}), dialog = $state(null), form = $state({}), roots = $state([]);
  let disposed = false, request;
  const base = '/media/nasctl';
  const status = $derived(view?.status);
  const tools = $derived(status?.tools || {});
  const shares = $derived(status?.shares || []);
  const netMounts = $derived(status?.net_mounts || []);
  const persisted = $derived(new Set(status?.persisted || []));
  const shareFolders = $derived([...roots.map((root) => root.path), ...(view?.disks || []).flatMap((disk) => disk.partitions.flatMap((part) => part.managed ? part.mountpoints : []))]);
  const fsLabel = {ext4:'ext4', ext3:'ext3', ext2:'ext2', xfs:'XFS', btrfs:'Btrfs', f2fs:'F2FS', vfat:'FAT32', exfat:'exFAT', ntfs:'NTFS', swap:'交换分区', zfs_member:'ZFS', linux_raid_member:'RAID 成员', crypto_LUKS:'加密卷'};
  const mountable = new Set(['ext4','ext3','ext2','xfs','btrfs','f2fs','vfat','exfat','ntfs']);
  const transport = (disk) => ({usb:'USB', sata:'SATA', nvme:'NVMe', mmc:'存储卡', ata:'SATA', scsi:'SCSI', virtio:'虚拟'})[disk.transport] || disk.transport || '';
  const percent = (part) => part.used != null && part.avail != null && part.used + part.avail > 0 ? part.used / (part.used + part.avail) * 100 : null;
  const tone = (value) => value >= 90 ? 'hot' : value >= 75 ? 'warm' : '';
  const host = typeof location === 'undefined' ? 'nas' : location.hostname;

  async function load() {
    request?.abort(); const current = new AbortController(); request = current; loading = true;
    try {
      const [data, settings] = await Promise.all([api('/api/v1/storage', {signal:current.signal}), api('/api/v1/settings', {signal:current.signal}).catch(() => null)]);
      if (disposed || request !== current) return;
      view = data; error = data.error || '';
      roots = settings?.saved?.storage || settings?.active?.storage || [];
    } catch (failure) { if (!disposed && request === current && failure.name !== 'AbortError') error = failure.message; }
    finally { if (!disposed && request === current) loading = false; }
  }
  async function act(action, body, done, timeoutMs = 50_000) {
    if (pending) return false; pending = true; dialogError = ''; message = '';
    try {
      await api('/api/v1/storage/' + action, json('POST', body, {headers:{'X-Nasctl-Confirm':'1'}, timeoutMs}));
      if (disposed) return false;
      dialog = null; message = done; await load(); return true;
    } catch (failure) {
      if (disposed) return false;
      if (failure.status === 408 && action === 'format') {dialog = null; message = '格式化仍在进行，较大的磁盘需要几分钟。稍后点“刷新”查看结果。'; return false;}
      if (dialog) dialogError = failure.message; else error = failure.message;
      return false;
    } finally { if (!disposed) pending = false; }
  }
  async function checkSmart(disk) {
    smart = {...smart, [disk.path]:{loading:true}};
    try { const result = await api('/api/v1/storage/smart?device=' + encodeURIComponent(disk.path), {timeoutMs:45_000}); if (!disposed) smart = {...smart, [disk.path]:result}; }
    catch (failure) { if (!disposed) smart = {...smart, [disk.path]:{error:failure.message}}; }
  }
  function health(result) {
    if (!result || result.loading) return null;
    if (result.error) return {tone:'neutral', text:'无法检查'};
    if (!result.available) return {tone:'neutral', text:'不支持健康检查'};
    if (result.passed === false || (result.reallocated_sectors || 0) > 0 || (result.pending_sectors || 0) > 0) return {tone:'failed', text: result.passed === false ? '健康检查未通过' : '有坏道迹象'};
    return {tone:'complete', text:'健康'};
  }
  function open(kind, values = {}) { dialogError = ''; form = values; dialog = kind; }
  function close() { if (!pending) dialog = null; }
  const saveShares = (list, done) => act('shares', {shares:list}, done);
  function submitShare(event) {
    event.preventDefault();
    const entry = {name:form.name.trim(), path:form.path.trim(), writable:form.writable, guest:form.guest};
    const list = form.index == null ? [...shares, entry] : shares.map((item, index) => index === form.index ? entry : item);
    saveShares(list, form.index == null ? `已共享「${entry.name}」。` : `已更新「${entry.name}」。`);
  }
  onMount(() => { load(); return () => {disposed = true; request?.abort();}; });
</script>

<header class="page-heading"><div><h1>存储与共享</h1><p class="muted">磁盘、USB 设备、局域网共享和网络存储。</p></div><button class="button secondary" disabled={loading || pending} onclick={load}><Icon name="refresh" size={16}/>刷新</button></header>
{#if error}<ErrorNotice message={error}/>{/if}{#if message}<div class="notice success" role="status">{message}</div>{/if}

{#if view && !view.helper}
  <section class="panel settings-card storage-setup">
    <header class="settings-card-head"><span class="settings-card-icon"><Icon name="shield" size={19}/></span><div><h2>启用存储助手</h2><p class="muted">nasctl 平时以普通用户运行。挂载、格式化和共享需要 root 权限，这些操作交给一个按需启动、做完就退出的存储助手。</p></div></header>
    <p>在 NAS 上以 root 执行一次（发布包里已经带有这些文件）：</p>
    <pre class="command-block">install -m 0644 systemd/nasctl-storage.socket systemd/nasctl-storage@.service \
  systemd/nasctl-storage-automount@.service /etc/systemd/system/
install -m 0644 udev/99-nasctl-automount.rules /etc/udev/rules.d/
install -m 0644 tmpfiles/nasctl.conf /etc/tmpfiles.d/
systemd-tmpfiles --create /etc/tmpfiles.d/nasctl.conf
systemctl daemon-reload &amp;&amp; udevadm control --reload
systemctl enable --now nasctl-storage.socket
systemctl restart nasctl-web</pre>
    <p class="muted">可选软件：<code>smartmontools</code>（硬盘健康）、<code>samba</code>（局域网共享）、<code>cifs-utils</code> / <code>nfs-common</code>（网络存储）、<code>exfatprogs</code>、<code>ntfs-3g</code>。详见 STORAGE.md。</p>
  </section>
{:else if view}
  <div class="segmented storage-tabs" role="tablist">
    <button role="tab" class:active={tab === 'disks'} aria-selected={tab === 'disks'} onclick={() => tab = 'disks'}>磁盘<span>{view.disks.length}</span></button>
    <button role="tab" class:active={tab === 'shares'} aria-selected={tab === 'shares'} onclick={() => tab = 'shares'}>局域网共享<span>{shares.length}</span></button>
    <button role="tab" class:active={tab === 'net'} aria-selected={tab === 'net'} onclick={() => tab = 'net'}>网络存储<span>{netMounts.length}</span></button>
  </div>

  {#if tab === 'disks'}
    <label class="switch-row panel storage-automount"><span><strong>插入 USB 硬盘时自动挂载</strong><small>挂载到 {base}/卷标，拔出后自动卸载。系统盘和不认识的文件系统不会被挂载。</small></span><input type="checkbox" role="switch" class="switch" checked={status?.automount} disabled={readOnly || pending || !status} onchange={(event) => act('automount', {enabled:event.currentTarget.checked}, event.currentTarget.checked ? '已开启自动挂载。' : '已关闭自动挂载。')}/></label>
    {#if !roots.some((root) => root.path === base || base.startsWith(root.path + '/'))}<p class="notice warning storage-hint"><Icon name="info" size={16}/><span>想在“文件”里浏览这里挂载的磁盘，请在 <a href="#admin/system">系统设置 › 存储位置</a> 添加目录 <code>{base}</code>，重启 nasctl 后生效；之后新挂载的磁盘会直接出现在里面。</span></p>{/if}
    <div class="drive-grid">
      {#each view.disks as disk (disk.path)}{@const result = smart[disk.path]}{@const state = health(result)}
        <article class="panel drive-card" class:system={disk.system}>
          <header>
            <span class="drive-icon" class:usb={disk.transport === 'usb' || disk.removable}><Icon name={disk.transport === 'usb' || disk.removable ? 'usb' : 'disk'} size={24}/></span>
            <div><strong>{disk.model || disk.vendor || disk.name}</strong><small class="muted">{bytes(disk.size)}{disk.model || disk.vendor ? ' · ' + disk.name : ''}{transport(disk) ? ' · ' + transport(disk) : ''}{disk.serial ? ' · ' + disk.serial : ''}</small></div>
            {#if disk.system}<span class="badge neutral">系统盘</span>{/if}
            {#if state}<span class={'badge ' + state.tone}>{state.text}</span>{/if}
          </header>
          {#if result && !result.loading && !result.error && result.available}<dl class="drive-health">
            {#if result.temperature_c != null}<div><dt>温度</dt><dd>{result.temperature_c}°C</dd></div>{/if}
            {#if result.power_on_hours != null}<div><dt>通电</dt><dd>{result.power_on_hours >= 48 ? Math.round(result.power_on_hours / 24) + ' 天' : result.power_on_hours + ' 小时'}</dd></div>{/if}
            {#if result.percent_used != null}<div><dt>寿命已用</dt><dd>{result.percent_used}%</dd></div>{/if}
            {#if result.reallocated_sectors != null}<div><dt>重映射扇区</dt><dd>{result.reallocated_sectors}</dd></div>{/if}
            {#if result.pending_sectors != null}<div><dt>待处理扇区</dt><dd>{result.pending_sectors}</dd></div>{/if}
          </dl>{:else if result?.detail || result?.error}<p class="field-help drive-note">{result.error || result.detail}</p>{/if}
          <ul class="partition-list">
            {#each disk.partitions as part (part.path)}{@const used = percent(part)}{@const mp = part.mountpoints[0]}
              <li>
                <div class="partition-main">
                  <strong>{part.label || part.name}</strong>
                  <small class="muted">{fsLabel[part.fstype] || part.fstype || '未格式化'} · {bytes(part.size)}{mp ? ' · ' + mp : ''}{persisted.has(mp) ? ' · 开机挂载' : ''}</small>
                  {#if used != null}<span class="widget-bar partition-bar"><i class={tone(used)} style:width={used + '%'}></i></span><small class="muted">{bytes(part.avail)} 可用</small>{/if}
                </div>
                {#if !readOnly && !part.system && !disk.system}
                  {#if part.managed}<button class="button secondary small" disabled={pending} onclick={() => open('unmount', {name:mp.slice(base.length + 1), forget:persisted.has(mp), label:part.label || part.name})}>卸载</button>
                  {:else if !mp && mountable.has(part.fstype)}<button class="button secondary small" disabled={pending} onclick={() => open('mount', {device:part.path, name:(part.label || '').replace(/[^A-Za-z0-9._-]/g, '').replace(/^[._-]+/, '').slice(0, 32), persist:!disk.removable && disk.transport !== 'usb', label:part.label || part.name})}>挂载</button>{/if}
                {/if}
              </li>
            {:else}<li class="muted">没有分区</li>{/each}
          </ul>
          <footer>
            <button class="text-button" disabled={result?.loading} onclick={() => checkSmart(disk)}>{result?.loading ? '正在检查…' : '检查健康'}</button>
            {#if !readOnly && !disk.system && !disk.read_only}<button class="text-button danger-text" disabled={pending} onclick={() => open('format', {device:disk.path, label:'data', confirm:'', want:disk.serial || disk.name, title:disk.model || disk.name, size:disk.size})}>格式化</button>{/if}
          </footer>
        </article>
      {:else}<p class="section-empty">没有找到磁盘。</p>{/each}
    </div>
  {:else if tab === 'shares'}
    {#if !tools.smbd}<p class="notice warning"><Icon name="warning" size={16}/><span>还没有安装 Samba。在 NAS 上执行 <code>apt install samba</code> 后刷新。</span></p>{/if}
    <section class="panel settings-card">
      <header class="settings-card-head"><span class="settings-card-icon"><Icon name="users" size={19}/></span><div><h2>共享账号</h2><p class="muted">需要密码的共享都用账号 <code>nasctl-share</code> 登录。文件以 nasctl 服务用户的身份读写，和网页里的权限一致。</p></div><span class={'badge ' + (status?.share_user ? 'complete' : 'neutral')}>{status?.share_user ? '已设置密码' : '未设置'}</span></header>
      {#if !readOnly}<button class="button secondary small" disabled={pending || !tools.smbd} onclick={() => open('password', {password:'', again:''})}>{status?.share_user ? '更改共享密码' : '设置共享密码'}</button>{/if}
    </section>
    <section class="panel settings-card">
      <header class="settings-card-head"><span class="settings-card-icon"><Icon name="share" size={19}/></span><div><h2>共享文件夹</h2><p class="muted">Windows 在资源管理器地址栏输入 <code>\\{host}</code>，macOS 在访达里按 ⌘K 输入 <code>smb://{host}</code>。</p></div>{#if !readOnly}<button class="button secondary small" disabled={pending || !tools.smbd} onclick={() => open('share', {name:'', path:shareFolders[0] || '', writable:false, guest:false, index:null})}><Icon name="plus" size={15}/>添加共享</button>{/if}</header>
      <ul class="share-list">
        {#each shares as share, index (share.name)}
          <li><span class="directory-icon"><Icon name="folder" size={18}/></span><div><strong>{share.name}</strong><small class="muted">{share.path} · {share.writable ? '可读写' : '只读'} · {share.guest ? '无需密码' : '需要密码'}</small></div>
            {#if !readOnly}<button class="text-button" disabled={pending} onclick={() => open('share', {...share, index})}>编辑</button><button class="text-button danger-text" disabled={pending} onclick={() => saveShares(shares.filter((_, i) => i !== index), `已停止共享「${share.name}」，文件不受影响。`)}>停止共享</button>{/if}
          </li>
        {:else}<li class="muted">还没有共享文件夹。</li>{/each}
      </ul>
    </section>
  {:else}
    <section class="panel settings-card">
      <header class="settings-card-head"><span class="settings-card-icon"><Icon name="cloud" size={19}/></span><div><h2>网络存储</h2><p class="muted">把另一台 NAS 或电脑上的 SMB / NFS 共享挂到 {base} 下，就能像本地磁盘一样浏览、备份。</p></div>{#if !readOnly}<button class="button secondary small" disabled={pending} onclick={() => open('net', {kind:'smb', name:'', server:'', share:'', username:'', password:'', read_only:false, persist:true})}><Icon name="plus" size={15}/>连接网络存储</button>{/if}</header>
      <ul class="share-list">
        {#each netMounts as item (item.name)}
          <li><span class="directory-icon"><Icon name="cloud" size={18}/></span><div><strong>{item.name}</strong><small class="muted">{item.kind === 'smb' ? `//${item.server}/${item.share}` : `${item.server}:/${item.share}`} · {base}/{item.name}{item.read_only ? ' · 只读' : ''}{item.persist ? ' · 开机连接' : ''}</small></div>
            <span class={'badge ' + (item.mounted ? 'complete' : 'neutral')}>{item.mounted ? '已连接' : '未连接'}</span>
            {#if !readOnly}<button class="text-button danger-text" disabled={pending} onclick={() => open('net-remove', {name:item.name})}>断开并移除</button>{/if}
          </li>
        {:else}<li class="muted">还没有网络存储。</li>{/each}
      </ul>
    </section>
  {/if}
{:else if loading}<p class="muted" role="status">正在读取磁盘…</p>{/if}

{#if dialog === 'mount'}<Dialog title={'挂载 ' + form.label} description={`挂载到 ${base} 下的一个文件夹。`} onclose={close}>
  <form class="form-grid" onsubmit={(event) => {event.preventDefault(); act('mount', {device:form.device, name:form.name, persist:form.persist}, `已挂载到 ${base}/${form.name || '…'}。`);}}>
    <label class="field">文件夹名称<input required pattern={'[A-Za-z0-9][A-Za-z0-9._\\-]{0,31}'} bind:value={form.name} placeholder="usb-disk"/><small class="field-help">{base}/{form.name || '名称'}；只能用字母、数字、点、下划线和短横线。</small></label>
    <label class="switch-row panel"><span><strong>开机自动挂载</strong><small>写入 /etc/fstab；磁盘不在时不会影响开机。</small></span><input type="checkbox" role="switch" class="switch" bind:checked={form.persist}/></label>
    {#if dialogError}<ErrorNotice message={dialogError}/>{/if}
    <div class="dialog-footer"><button type="button" class="button secondary" disabled={pending} onclick={close}>取消</button><button class="button primary" disabled={pending}>{pending ? '正在挂载…' : '挂载'}</button></div>
  </form>
</Dialog>{/if}

{#if dialog === 'unmount'}<Dialog title={'卸载 ' + form.label} onclose={close}>
  <p>卸载前请确认没有正在复制或播放的文件。卸载后可以安全拔出设备。</p>
  <label class="switch-row panel"><span><strong>同时取消开机自动挂载</strong><small>不勾选时，下次开机仍会自动挂载。</small></span><input type="checkbox" role="switch" class="switch" bind:checked={form.forget}/></label>
  {#if dialogError}<ErrorNotice message={dialogError}/>{/if}
  <div class="dialog-footer"><button class="button secondary" disabled={pending} onclick={close}>取消</button><button class="button primary" disabled={pending} onclick={() => act('unmount', {name:form.name, forget:form.forget}, '已卸载，可以拔出设备了。')}>{pending ? '正在卸载…' : '卸载'}</button></div>
</Dialog>{/if}

{#if dialog === 'format'}<Dialog title={'格式化 ' + form.title} description={bytes(form.size) + ' · ' + form.device} onclose={close}>
  <div class="notice error"><Icon name="warning" size={16}/><span>这块磁盘上的<strong>所有数据都会被永久删除</strong>，无法恢复。格式化为 ext4，并挂载到 {base}/{form.label || '卷标'}。</span></div>
  <form class="form-grid" onsubmit={(event) => {event.preventDefault(); act('format', {device:form.device, label:form.label, confirm:form.confirm}, `格式化完成，已挂载到 ${base}/${form.label}。`, 55_000);}}>
    <label class="field">卷标<input required pattern={'[A-Za-z0-9][A-Za-z0-9_\\-]{0,15}'} bind:value={form.label}/></label>
    <label class="field"><span>输入 <code>{form.want}</code> 以确认</span><input required autocomplete="off" bind:value={form.confirm}/></label>
    {#if dialogError}<ErrorNotice message={dialogError}/>{/if}
    <div class="dialog-footer"><button type="button" class="button secondary" disabled={pending} onclick={close}>取消</button><button class="button primary danger" disabled={pending || form.confirm !== form.want}>{pending ? '正在格式化…' : '永久删除并格式化'}</button></div>
  </form>
</Dialog>{/if}

{#if dialog === 'password'}<Dialog title="共享密码" description="用户名 nasctl-share" onclose={close}>
  <form class="form-grid" onsubmit={(event) => {event.preventDefault(); if (form.password !== form.again) {dialogError = '两次输入的密码不一致'; return;} act('share-password', {password:form.password}, '共享密码已设置。');}}>
    <label class="field">新密码<input type="password" required minlength="8" maxlength="128" autocomplete="new-password" bind:value={form.password}/></label>
    <label class="field">再输入一次<input type="password" required minlength="8" maxlength="128" autocomplete="new-password" bind:value={form.again}/></label>
    {#if dialogError}<ErrorNotice message={dialogError}/>{/if}
    <div class="dialog-footer"><button type="button" class="button secondary" disabled={pending} onclick={close}>取消</button><button class="button primary" disabled={pending}>{pending ? '正在保存…' : '保存密码'}</button></div>
  </form>
</Dialog>{/if}

{#if dialog === 'share'}<Dialog title={form.index == null ? '添加共享' : '编辑共享'} onclose={close}>
  <form class="form-grid" onsubmit={submitShare}>
    <label class="field">共享名称<input required maxlength="32" bind:value={form.name} placeholder="照片"/><small class="field-help">在电脑上看到的文件夹名。</small></label>
    <label class="field">NAS 上的文件夹<input required list="share-folders" bind:value={form.path} placeholder="/srv/data/photos"/><small class="field-help">必须在已配置的存储位置或 {base} 里面。</small></label>
    <datalist id="share-folders">{#each shareFolders as folder}<option value={folder}></option>{/each}</datalist>
    <div class="switch-list">
      <label class="switch-row"><span><strong>允许写入</strong><small>关闭时电脑只能读取和复制。</small></span><input type="checkbox" role="switch" class="switch" bind:checked={form.writable}/></label>
      <label class="switch-row"><span><strong>无需密码访问</strong><small>局域网里的任何人都能打开，只建议用于公开资料。</small></span><input type="checkbox" role="switch" class="switch" bind:checked={form.guest}/></label>
    </div>
    {#if !form.guest && !status?.share_user}<p class="field-help">需要密码的共享要先设置共享密码。</p>{/if}
    {#if dialogError}<ErrorNotice message={dialogError}/>{/if}
    <div class="dialog-footer"><button type="button" class="button secondary" disabled={pending} onclick={close}>取消</button><button class="button primary" disabled={pending}>{pending ? '正在保存…' : '保存'}</button></div>
  </form>
</Dialog>{/if}

{#if dialog === 'net'}<Dialog title="连接网络存储" onclose={close}>
  <form class="form-grid" onsubmit={(event) => {event.preventDefault(); act('net-add', {net:{name:form.name, kind:form.kind, server:form.server.trim(), share:form.share.trim(), username:form.kind === 'smb' ? form.username : '', password:form.kind === 'smb' ? form.password : '', read_only:form.read_only, persist:form.persist}}, `已连接到 ${base}/${form.name}。`);}}>
    <div class="segmented"><button type="button" class:active={form.kind === 'smb'} onclick={() => form.kind = 'smb'}>SMB（Windows / NAS）</button><button type="button" class:active={form.kind === 'nfs'} onclick={() => form.kind = 'nfs'}>NFS</button></div>
    <label class="field">服务器地址<input required bind:value={form.server} placeholder="192.168.1.20"/></label>
    <label class="field">{form.kind === 'smb' ? '共享名称' : '导出路径'}<input required bind:value={form.share} placeholder={form.kind === 'smb' ? 'media' : '/export/media'}/></label>
    {#if form.kind === 'smb'}<label class="field">用户名<input autocomplete="off" bind:value={form.username} placeholder="留空表示访客"/></label><label class="field">密码<input type="password" autocomplete="new-password" bind:value={form.password}/><small class="field-help">只保存在 NAS 的 root 专属文件里，网页不会再显示。</small></label>{/if}
    <label class="field">本地名称<input required pattern={'[A-Za-z0-9][A-Za-z0-9._\\-]{0,31}'} bind:value={form.name} placeholder="office-nas"/><small class="field-help">挂载到 {base}/{form.name || '名称'}</small></label>
    <div class="switch-list">
      <label class="switch-row"><span><strong>开机自动连接</strong></span><input type="checkbox" role="switch" class="switch" bind:checked={form.persist}/></label>
      <label class="switch-row"><span><strong>只读</strong></span><input type="checkbox" role="switch" class="switch" bind:checked={form.read_only}/></label>
    </div>
    {#if dialogError}<ErrorNotice message={dialogError}/>{/if}
    <div class="dialog-footer"><button type="button" class="button secondary" disabled={pending} onclick={close}>取消</button><button class="button primary" disabled={pending}>{pending ? '正在连接…' : '连接'}</button></div>
  </form>
</Dialog>{/if}

{#if dialog === 'net-remove'}<Dialog title={'移除 ' + form.name} onclose={close}>
  <p>断开连接并删除保存的账号信息。远端的文件不受影响。</p>
  {#if dialogError}<ErrorNotice message={dialogError}/>{/if}
  <div class="dialog-footer"><button class="button secondary" disabled={pending} onclick={close}>取消</button><button class="button primary" disabled={pending} onclick={() => act('net-remove', {name:form.name}, `已移除「${form.name}」。`)}>{pending ? '正在断开…' : '断开并移除'}</button></div>
</Dialog>{/if}
