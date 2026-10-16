<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api,json } from '../lib/api.js';
  import { bytes } from '../lib/format.js';
  import Dialog from './Dialog.svelte';
  import Icon from './Icon.svelte';
  let snapshot=$state(null),draft=$state(null),secrets=$state({}),error=$state(''),message=$state(''),pending=$state(false),checked=$state(null),confirm=$state(false),readOnly=$state(true),baseline=$state('');
  let disposed=false,controller;
  // Each budget is [key, label, unit]; byte and memory fields show their size in words.
  const fileGroups=[
    {title:'浏览与搜索',fields:[['max_entries','目录读取条目'],['max_page_size','最大每页条目'],['default_page_size','默认每页条目'],['search_max_results','搜索结果条目'],['search_max_examined','搜索检查条目']]},
    {title:'预览与上传',fields:[['preview_max_bytes','预览上限（字节）','B'],['upload_max_bytes','上传上限（字节）','B']]},
    {title:'复制、批量与回收站',fields:[['copy_max_entries','复制条目'],['copy_max_bytes','复制上限（字节）','B'],['batch_max_items','批量选择条目'],['trash_max_entries','回收站条目']]},
  ];
  const guardFields=[['start_kb','开始所需可用内存（KiB）','KiB'],['warn_kb','内存警告阈值（KiB）','KiB'],['stop_kb','内存停止阈值（KiB）','KiB'],['worker_high_mb','处理组件内存警告（MiB）','MiB'],['worker_max_mb','处理组件内存上限（MiB）','MiB']];
  const jobFields=[['max_open_jobs','最多开放任务'],['max_history','保留任务记录'],['audit_retention_days','审计保留天数']];
  const secretLabels={username:'下载器用户名',password:'下载器密码',tmdb_token:'TMDB 令牌'};
  const scale={B:1,KiB:1024,MiB:1024**2};
  const size=(value,unit)=>Number(value)>0?'≈ '+bytes(Number(value)*scale[unit]):'';
  const payload=()=>({revision:snapshot.revision,settings:JSON.parse(JSON.stringify(draft)),secrets:JSON.parse(JSON.stringify(secrets))});
  const signature=()=>draft?JSON.stringify(payload()):'';
  const dirty=$derived(Boolean(draft)&&signature()!==baseline);
  const fresh=()=>Object.fromEntries(Object.keys(secretLabels).map(key=>[key,{action:'keep',value:''}]));
  $effect(()=>{const value=signature();if(checked&&checked.signature!==value)checked=null;});
  function adopt(data){snapshot=data;draft=JSON.parse(JSON.stringify(data.saved));secrets=fresh();checked=null;baseline=signature();}
  async function load(){if(pending)return;pending=true;error='';controller?.abort();controller=new AbortController();try{const [data,overview]=await Promise.all([api('/api/v1/settings',{signal:controller.signal}),api('/api/v1/overview',{signal:controller.signal})]);if(disposed)return;adopt(data);readOnly=overview.read_only;message='';}catch(e){if(!disposed&&e.name!=='AbortError')error=e.message;}finally{if(!disposed)pending=false;}}
  async function validate(){if(pending)return;pending=true;error='';message='';try{const sent=payload();const sig=signature();const result=await api('/api/v1/settings/validate',json('POST',sent));if(disposed)return;checked={signature:sig,request:sent,result};message='校验通过，请核对后保存。保存后需要重启 nasctl 才会应用。';}catch(e){if(!disposed)error=e.message;}finally{if(!disposed)pending=false;}}
  async function save(){if(pending||!checked||checked.signature!==signature())return;pending=true;error='';try{const result=await api('/api/v1/settings',json('POST',checked.request,{headers:{'X-Nasctl-Confirm':'1'}}));if(disposed)return;adopt(result);confirm=false;message='配置已保存。当前服务继续使用原运行值，下次重启生效。';}catch(e){if(!disposed)error=e.message;}finally{if(!disposed)pending=false;}}
  onMount(()=>{load();return()=>{disposed=true;controller?.abort();};});
</script>
{#snippet number(object,key,label,unit,attrs={})}
  <label class="field">{label}<input type="number" min="1" step="1" aria-label={label} bind:value={object[key]} {...attrs}/>{#if unit}<small class="field-help">{size(object[key],unit)}</small>{/if}</label>
{/snippet}
<div class="nas-settings" aria-label="统一设备设置">
  {#if error}<ErrorNotice message={error}/>{/if}{#if message}<p class="notice success" role="status">{message}</p>{/if}
  {#if snapshot?.restart_required}<p class="notice warning" role="status"><Icon name="refresh" size={16}/>已有配置等待重启。这里编辑的是已保存值；当前任务仍按启动时的配置运行。</p>{/if}
  {#if draft}
    <fieldset disabled={pending||readOnly}>
      <section id="settings-storage" class="panel settings-card">
        <header class="settings-card-head"><span class="settings-card-icon"><Icon name="disk" size={19}/></span><div><h2>存储位置</h2><p class="muted">文件页、相册和备份使用的目录。默认只读；打开“允许文件整理”后，操作者才能上传、移动和删除。移除配置不会删除文件。</p></div><button class="button secondary small" disabled={draft.storage.length>=64} onclick={()=>draft.storage=[...draft.storage,{id:'',label:'',path:'',writable:false,expose_hidden:false}]}><Icon name="plus" size={15}/>添加存储目录</button></header>
        <div class="root-list">
          {#each draft.storage as root,index}
            <div class="nas-root-card">
              <div class="root-card-head"><span class="directory-icon"><Icon name="disk" size={18}/></span><div><strong>{root.label||root.id||'新的存储目录'}</strong><small class="muted">{root.writable?'允许整理':'只读'}{root.expose_hidden?' · 显示隐藏文件':''}</small></div><button class="text-button danger-text" onclick={()=>draft.storage=draft.storage.filter((_,i)=>i!==index)}>移除目录配置</button></div>
              <div class="settings-fields"><label class="field">目录 ID<input aria-label={'存储目录 ID '+(index+1)} bind:value={root.id} maxlength="32" placeholder="data"/></label><label class="field">显示名称<input aria-label={'存储目录名称 '+(index+1)} bind:value={root.label} maxlength="128" placeholder="家庭文件"/></label><label class="field wide">本机绝对路径<input class="mono-input" aria-label={'存储目录路径 '+(index+1)} bind:value={root.path} maxlength="1024" placeholder="/mnt/data/files"/></label></div>
              <div class="nas-root-actions switch-list compact"><label class="switch-row"><span><strong>允许文件整理</strong><small>上传、新建、移动、回收与恢复</small></span><input type="checkbox" role="switch" class="switch" bind:checked={root.writable}/></label><label class="switch-row"><span><strong>显示隐藏文件</strong><small>以“.”开头的文件和文件夹</small></span><input type="checkbox" role="switch" class="switch" bind:checked={root.expose_hidden}/></label></div>
            </div>
          {/each}
          {#if !draft.storage.length}<p class="section-empty">还没有存储目录。添加一个 NAS 上已经准备好的目录。</p>{/if}
        </div>
      </section>

      <section id="settings-files" class="panel settings-card">
        <header class="settings-card-head"><span class="settings-card-icon"><Icon name="transfer" size={19}/></span><div><h2>文件与传输</h2><p class="muted">单次操作的上限。数值越大越方便，但在内存较小的设备上会占用更多资源。</p></div></header>
        {#each fileGroups as group}<div class="settings-group"><h3>{group.title}</h3><div class="settings-fields">{#each group.fields as [key,label,unit]}{@render number(draft.files,key,label,unit)}{/each}</div></div>{/each}
      </section>

      <section id="settings-anime" class="panel settings-card">
        <header class="settings-card-head"><span class="settings-card-icon"><Icon name="anime" size={19}/></span><div><h2>追番与下载器</h2><p class="muted">连接 qBittorrent，按订阅自动下载并整理到媒体库。</p></div></header>
        <div class="switch-list"><label class="switch-row"><span><strong>启用原生追番</strong><small>关闭后订阅和媒体库保留，但不再刷新</small></span><input type="checkbox" role="switch" class="switch" bind:checked={draft.anime.enabled}/></label><label class="switch-row"><span><strong>开启定时刷新与同步</strong><small>按下面的间隔自动检查 RSS 并同步下载状态</small></span><input type="checkbox" role="switch" class="switch" bind:checked={draft.anime.auto_poll}/></label></div>
        <div class="settings-group"><h3>下载器与目录</h3><div class="settings-fields"><label class="field wide">qBittorrent WebUI 地址<input aria-label="下载器地址" bind:value={draft.anime.qbittorrent_url} placeholder="http://127.0.0.1:8080"/></label><label class="field">本地下载目录<input class="mono-input" aria-label="本地下载目录" bind:value={draft.anime.download_dir}/></label><label class="field">下载器内部目录<input class="mono-input" aria-label="下载器内部目录" bind:value={draft.anime.qbittorrent_download_dir}/><small class="field-help">下载器在容器里看到的路径，不在容器中运行可留空</small></label><label class="field wide">媒体库目录<input class="mono-input" aria-label="媒体库目录" bind:value={draft.anime.library_dir}/></label>{@render number(draft.anime,'poll_interval_seconds','RSS 刷新间隔（秒）','',{min:60,'aria-label':'RSS 刷新间隔'})}{@render number(draft.anime,'max_items_per_feed','每个订阅读取条目','',{max:500,'aria-label':'RSS 条目上限'})}</div></div>
        <div class="settings-group"><h3>账号与令牌</h3><p class="field-help">已保存的值不会显示。选择“设置新值”后输入，保存时替换。</p>
          <div class="secret-list">{#each Object.entries(secretLabels) as [key,label]}<div class="nas-secret"><div class="secret-name"><strong>{label}</strong><span class="badge" class:neutral={!snapshot.secrets[key]}>{snapshot.secrets[key]?'已设置':'未设置'}</span></div><select aria-label={label+'操作'} bind:value={secrets[key].action} onchange={()=>{if(secrets[key].action!=='set')secrets[key].value='';}}><option value="keep">保留现有值</option><option value="set">设置新值</option><option value="clear">清除</option></select>{#if secrets[key].action==='set'}<input aria-label={'新'+label} type="password" placeholder={'新'+label} autocomplete="new-password" maxlength="4096" bind:value={secrets[key].value}/>{/if}</div>{/each}</div>
        </div>
      </section>

      <section id="settings-guard" class="panel settings-card">
        <header class="settings-card-head"><span class="settings-card-icon"><Icon name="gauge" size={19}/></span><div><h2>资源保护与记录</h2><p class="muted">可用内存低于阈值时，扫描和缩略图等重任务会暂停或停止，保证设备不被拖垮。</p></div></header>
        <div class="settings-group"><h3>内存阈值</h3><p class="field-help">需满足“停止阈值 &lt; 警告阈值 &lt; 开始阈值”；处理组件内存上限不得小于解码预算。</p><div class="settings-fields">{#each guardFields as [key,label,unit]}{@render number(draft.guard_memory,key,label,unit)}{/each}</div></div>
        <div class="settings-group"><h3>任务与审计</h3><div class="settings-fields">{#each jobFields as [key,label]}{@render number(draft.jobs,key,label,'')}{/each}</div></div>
      </section>

      <div class="save-bar" class:dirty>
        <span class="save-bar-state">{#if checked}<Icon name="check" size={16}/>已校验，可以保存{:else if dirty}<span class="status-dot working"></span>有未保存的修改{:else}<Icon name="check" size={16}/>与已保存的配置一致{/if}<small>保存后重启 nasctl 生效</small></span>
        <div class="button-group"><button class="button secondary" disabled={pending||!dirty} onclick={load} title="放弃修改，重新读取已保存的配置">放弃修改</button><button class="button secondary" onclick={validate}>校验配置</button><button class="button primary" disabled={!checked||checked.signature!==signature()} onclick={()=>confirm=true}>保存配置…</button></div>
      </div>
    </fieldset>
    {#if readOnly}<p class="field-help">当前控制台只读，配置修改不可用。</p>{/if}
    <details class="panel settings-card nas-active"><summary class="settings-card-head"><span class="settings-card-icon"><Icon name="info" size={19}/></span><div><h2>当前运行值</h2><p class="muted">本次启动时生效的配置</p></div><Icon name="chevron" size={16} class="summary-chevron"/></summary><dl class="key-values"><dt>存储目录</dt><dd>{snapshot.active.storage.map(root=>root.label||root.id).join('、')||'未配置'}</dd><dt>上传上限</dt><dd>{bytes(snapshot.active.files.upload_max_bytes)}</dd><dt>复制上限</dt><dd>{bytes(snapshot.active.files.copy_max_bytes)}</dd><dt>原生追番</dt><dd>{snapshot.active.anime.enabled?'启用':'关闭'} · {snapshot.active.anime.auto_poll?'定时调度':'手动调度'}</dd><dt>开放任务上限</dt><dd>{snapshot.active.jobs.max_open_jobs}</dd><dt>任务历史上限</dt><dd>{snapshot.active.jobs.max_history}</dd><dt>处理组件内存上限</dt><dd>{snapshot.active.guard_memory.worker_max_mb} MiB</dd></dl></details>
  {:else if pending}<p class="section-empty" role="status">正在读取配置…</p>{/if}
</div>
{#if confirm&&checked}<Dialog title="保存设备配置" onclose={()=>{if(!pending)confirm=false;}}><p>保存 {checked.request.settings.storage.length} 个存储目录，以及文件、追番和资源预算。已选“清除”的秘密将删除，已选“设置”的值将替换。</p><p class="muted">当前运行任务继续使用原配置。请在任务停止后重启 nasctl，使新目录和预算生效。</p><div class="dialog-footer"><button class="button secondary" disabled={pending} onclick={()=>confirm=false}>返回修改</button><button class="button primary" disabled={pending} onclick={save}>{pending?'正在保存…':'确认保存，下次启动生效'}</button></div>{#if error}<ErrorNotice message={error}/>{/if}</Dialog>{/if}
