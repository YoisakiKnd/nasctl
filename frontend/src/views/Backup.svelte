<script>
  import { onMount } from 'svelte';
  import { api,json,query } from '../lib/api.js';
  import { bytes,time } from '../lib/format.js';
  import Icon from '../components/Icon.svelte';
  import Dialog from '../components/Dialog.svelte';
  import TaskMonitor from '../components/TaskMonitor.svelte';
  import BackupResult from '../components/BackupResult.svelte';
  let {readOnly=false}=$props();
  let settings=$state({plans:[],digests:{},busy:false}),roots=$state([]),limits=$state({copy_max_entries:10000,copy_max_bytes:8*1024**3});
  let loading=$state(true),error=$state(''),historyError=$state(''),message=$state(''),historyRoot=$state(''),history=$state({snapshots:[],incomplete:[]});
  let modal=$state(null),draft=$state(null),pending=$state(false),dialogError=$state(''),confirmation=$state(''),toRoot=$state(''),destination=$state('');
  let task=$state(''),taskState=$state(''),taskOperation=$state(''),result=$state(null);
  let request,historyRequest,submitRequest,disposed=false,generation=0,historyGeneration=0;
  const running=$derived(['queued','running','waiting','paused'].includes(taskState));
  const blocked=$derived(readOnly||settings.busy||running||pending);
  const rootName=(id)=>roots.find((r)=>r.id===id)?.label||id;
  const name=(kind)=>({run:'执行备份',verify:'校验备份',restore:'恢复文件',delete:'永久删除备份',cleanup:'清理不完整备份',remove:'移除备份计划',plan:'保存备份计划'})[kind];
  async function load(){
    const current=++generation;request?.abort();request=new AbortController();loading=true;error='';
    try{
      const [data,storage]=await Promise.all([api('/api/v1/backup/settings',{signal:request.signal}),api('/api/v1/files/roots',{signal:request.signal})]);
      if(disposed||current!==generation)return;settings=data;roots=storage.roots||[];limits={...limits,...storage.limits};
      if(!historyRoot||!roots.some((r)=>r.id===historyRoot))historyRoot=data.plans[0]?.to_root||roots.find((r)=>r.writable&&r.available)?.id||roots[0]?.id||'';
      if(historyRoot)loadHistory();
    }catch(failure){if(!disposed&&current===generation&&failure.name!=='AbortError')error=failure.message;}
    finally{if(!disposed&&current===generation)loading=false;}
  }
  async function loadHistory(){
    if(!historyRoot)return;const current=++historyGeneration;historyRequest?.abort();historyRequest=new AbortController();historyError='';
    try{const data=await api(query('/api/v1/backup/history',{root:historyRoot}),{signal:historyRequest.signal});if(!disposed&&current===historyGeneration)history=data;}
    catch(failure){if(!disposed&&current===historyGeneration&&failure.name!=='AbortError')historyError=failure.message;}
  }
  function edit(plan=null){
    const first=roots.find((r)=>r.available)?.id||'';
    draft=plan?{...plan}:{id:'',name:'',root:first,path:'.',to_root:roots.find((r)=>r.writable&&r.available&&r.id!==first)?.id||'',limit:Math.min(10000,limits.copy_max_entries),max_bytes:limits.copy_max_bytes,keep:7,max_runtime:'30m'};
    modal={kind:'plan',existing:!!plan};dialogError='';confirmation='';
  }
  function open(kind,item){
    modal={kind,item};dialogError='';confirmation='';
    toRoot=roots.find((r)=>r.writable&&r.available&&r.id!==historyRoot)?.id||roots.find((r)=>r.writable&&r.available)?.id||'';
    destination='恢复-'+(item?.plan_id||'文件')+'-'+Date.now();
  }
  function close(){if(!pending){modal=null;draft=null;dialogError='';}}
  async function submit(event){
    event.preventDefault();if(pending||readOnly||running)return;
    pending=true;dialogError='';submitRequest=new AbortController();
    try{
      let path,body,method='POST';const kind=modal.kind,item=modal.item;
      if(kind==='plan'){path='/api/v1/backup/plans';body={...draft,limit:Number(draft.limit),max_bytes:Number(draft.max_bytes),keep:Number(draft.keep)};}
      else if(kind==='remove'){path='/api/v1/backup/plans/'+encodeURIComponent(item.id);body={};method='DELETE';}
      else if(kind==='run'){path='/api/v1/backup/run';body={plan:item.id,plan_sha256:settings.digests[item.id]};}
      else{
        path='/api/v1/backup/'+kind;body={root:historyRoot,id:typeof item==='string'?item:item.id};
        if(kind==='restore'){body.to_root=toRoot;body.to=destination;body.manifest_sha256=item.manifest_sha256;}
        if(kind==='delete'||kind==='cleanup'){if(confirmation!==body.id)throw new Error('请输入完整版本编号确认');body.confirm_id=confirmation;if(kind==='delete')body.manifest_sha256=item.manifest_sha256;}
      }
      const data=await api(path,json(method,body,{headers:{'X-Nasctl-Confirm':'1'},signal:submitRequest.signal}));
      if(disposed)return;
      modal=null;draft=null;message='';
      if(data.job_id){task=data.job_id;taskState='queued';taskOperation='backup.'+kind;result=null;}
      else{settings=data;message=kind==='remove'?'计划已移除，已有备份版本保留。':'备份计划已保存。';load();}
    }catch(failure){if(!disposed&&failure.name!=='AbortError')dialogError=failure.message;}
    finally{if(!disposed)pending=false;}
  }
  function updateTask(data){
    if(data.job?.id!==task)return;taskState=data.job.state;taskOperation=data.job.op;
    for(const event of [...(data.events||[])].reverse()){if(!event.note?.startsWith('{'))continue;try{const parsed=JSON.parse(event.note);if(parsed.kind==='backup.result'){result=parsed.result;break;}}catch{/* Plain task note. */}}
  }
  onMount(()=>{load();return()=>{disposed=true;request?.abort();historyRequest?.abort();submitRequest?.abort();};});
</script>
<header class="page-heading"><div><p class="eyebrow">本地文件保护</p><h1>备份</h1><p class="muted">为重要文件保留一份可校验的副本。</p></div><button class="button secondary" disabled={loading} onclick={load}><Icon name="refresh" size={16}/>刷新</button></header>
{#if error}<p class="notice error" role="alert">{error}<button class="text-button" onclick={load}>重试</button></p>{/if}
{#if message}<p class="notice success" role="status">{message}</p>{/if}
{#if task}<section class="panel task-panel"><TaskMonitor id={task} onupdate={updateTask} onsettled={load}/>{#if result}<BackupResult {result} operation={taskOperation} state={taskState}/>{/if}<a class="text-button" href="#jobs">在任务中心查看记录</a></section>{/if}
<section class="panel backup-plans">
  <div class="panel-heading"><div><h2>备份计划</h2><p class="field-help">手动执行，包含隐藏文件；不跟随符号链接，也不复制系统保留目录。</p></div><button class="button primary small" disabled={blocked||roots.filter((r)=>r.available).length<2||!roots.some((r)=>r.writable&&r.available)||settings.plans.length>=16} onclick={()=>edit()}><Icon name="plus" size={15}/>新建计划</button></div>
  {#if loading&&!settings.plans.length}<p role="status">正在读取备份配置…</p>
  {:else if !settings.plans.length}<div class="empty-state"><Icon name="backup" size={36}/><h3>创建第一份文件备份</h3><p>选择原文件所在目录和独立的备份存储位置。建议使用另一块磁盘，保护磁盘故障时的数据。</p>{#if roots.filter((r)=>r.available).length<2||!roots.some((r)=>r.writable&&r.available)}<p>先配置一个可写的备份存储位置，再创建计划。</p><a class="text-button" href="#settings">查看存储配置</a>{/if}</div>
  {:else}<div class="backup-plan-grid">{#each settings.plans as plan (plan.id)}<article class="backup-plan"><div class="backup-plan-heading"><span class="app-icon amber"><Icon name="backup" size={22}/></span><div><h3>{plan.name}</h3><small class="muted">{plan.id}</small></div></div><dl><dt>原文件</dt><dd>{rootName(plan.root)} / {plan.path}</dd><dt>备份位置</dt><dd>{rootName(plan.to_root)}</dd><dt>单次预算</dt><dd>{plan.limit.toLocaleString()} 项 · {bytes(plan.max_bytes)} · {plan.max_runtime}</dd><dt>保留策略</dt><dd>最近 {plan.keep} 份，成功后清理旧版本</dd></dl><div class="button-group"><button class="button primary small" disabled={blocked} onclick={()=>open('run',plan)}>执行备份</button><button class="text-button" disabled={blocked} onclick={()=>edit(plan)}>编辑</button><button class="text-button" disabled={blocked} onclick={()=>open('remove',plan)}>移除计划</button></div></article>{/each}</div>{/if}
</section>
<section class="panel backup-history"><div class="panel-heading"><h2>备份版本</h2><select aria-label="备份历史存储目录" bind:value={historyRoot} onchange={loadHistory} disabled={running}>{#each roots as root}<option value={root.id} disabled={!root.available}>{root.label||root.id}</option>{/each}</select></div>
  {#if historyError}<p class="notice error" role="alert">{historyError}<button class="text-button" onclick={loadHistory}>重试</button></p>{/if}
  {#if history.truncated}<p class="notice">版本目录达到读取上限，请先清理已确认的旧版本或不完整记录。</p>{/if}
  {#if !history.snapshots.length&&!history.incomplete.length}<div class="empty-state"><Icon name="clock" size={30}/><h3>还没有备份版本</h3><p>执行计划后，通过校验的版本会保存在这里。</p></div>{/if}
  {#each history.snapshots as snapshot (snapshot.id)}<article class="backup-version"><div><h3>{snapshot.name}</h3><p class="field-help">{time(snapshot.created_at)} · {snapshot.entries} 项 · {bytes(snapshot.bytes)} · {snapshot.deleting?'清理未完成':'创建时校验通过'}</p><small class="mono">{snapshot.id}</small><p class="field-help">来源：{rootName(snapshot.root)} / {snapshot.path}</p></div><div class="button-group">{#if snapshot.deleting}<button class="button secondary small" disabled={blocked} onclick={()=>open('cleanup',snapshot.id)}>继续清理</button>{:else}<button class="button secondary small" disabled={blocked} onclick={()=>open('verify',snapshot)}>校验</button><button class="button primary small" disabled={blocked||!roots.some((r)=>r.writable&&r.available)} onclick={()=>open('restore',snapshot)}>恢复</button><button class="text-button danger-text" disabled={blocked||!roots.find((r)=>r.id===historyRoot)?.writable} onclick={()=>open('delete',snapshot)}>删除副本</button>{/if}</div></article>{/each}
  {#each history.incomplete as id}<article class="backup-version"><div><h3>不完整备份</h3><p class="field-help">复制或校验未完成，不能作为可恢复版本。</p><small class="mono">{id}</small></div><button class="button secondary small" disabled={blocked||!roots.find((r)=>r.id===historyRoot)?.writable} onclick={()=>open('cleanup',id)}>清理</button></article>{/each}
</section>
{#if modal}<Dialog title={name(modal.kind)} onclose={close} wide={modal.kind==='plan'}><form class="backup-form" onsubmit={submit}>
  {#if modal.kind==='plan'}
    <div class="form-grid"><label>计划编号<input aria-label="备份计划编号" bind:value={draft.id} pattern={'[a-z0-9][a-z0-9_\\-]{0,31}'} maxlength="32" required disabled={pending||modal.existing}/></label><label>显示名称<input aria-label="备份计划名称" bind:value={draft.name} maxlength="128" required disabled={pending}/></label>
    <label>原文件存储目录<select aria-label="备份源存储目录" bind:value={draft.root} required disabled={pending}>{#if !roots.some((r)=>r.id===draft.root)}<option value={draft.root} disabled>{draft.root}（未配置）</option>{/if}{#each roots as root}<option value={root.id} disabled={!root.available}>{root.label||root.id}</option>{/each}</select></label><label>原文件相对位置<input aria-label="备份源相对位置" bind:value={draft.path} required maxlength="1024" disabled={pending}/></label>
    <label>备份存储目录<select aria-label="备份目标存储目录" bind:value={draft.to_root} required disabled={pending}>{#if !roots.some((r)=>r.id===draft.to_root)}<option value={draft.to_root} disabled>{draft.to_root||'请选择'}</option>{/if}{#each roots.filter((r)=>r.writable) as root}<option value={root.id} disabled={!root.available||root.id===draft.root}>{root.label||root.id}</option>{/each}</select></label><label>保留份数<input aria-label="备份保留份数" type="number" min="1" max="32" bind:value={draft.keep} required disabled={pending}/></label>
    <label>单次条目上限<input aria-label="备份条目上限" type="number" min="1" max={limits.copy_max_entries} bind:value={draft.limit} required disabled={pending}/></label><label>单次字节上限<input aria-label="备份字节上限" type="number" min="1" max={limits.copy_max_bytes} bind:value={draft.max_bytes} required disabled={pending}/></label>
    <label>最长运行时间<select aria-label="备份最长运行时间" bind:value={draft.max_runtime} disabled={pending}><option value="1m">1 分钟</option><option value="10m">10 分钟</option><option value="30m">30 分钟</option><option value="2h">2 小时</option></select></label></div>
    <p class="field-help">相对位置填 . 表示整个原文件根目录。备份保存为独立副本；源与目标需属于不同的存储目录。</p><p class="notice">新版本完成校验后，自动永久清理超出保留份数的旧备份。清理不会修改原文件，也无法从回收站找回旧副本。</p>
  {:else if modal.kind==='run'}<p>将 {rootName(modal.item.root)} / {modal.item.path} 备份到 {rootName(modal.item.to_root)}。</p><p>最多 {modal.item.limit.toLocaleString()} 项、{bytes(modal.item.max_bytes)}、{modal.item.max_runtime}。保留最近 {modal.item.keep} 份，新版本验证通过后清理旧版本。</p><p class="field-help">任务在后台执行，关闭窗口后继续。可以从任务卡片明确停止。</p>
  {:else if modal.kind==='remove'}<p>移除“{modal.item.name}”的计划配置。已有备份版本仍然保留，可以在历史中恢复或单独删除。</p>
  {:else if modal.kind==='verify'}<p>读取“{modal.item.name}”的清单和全部文件，核对 {modal.item.entries} 项、{bytes(modal.item.bytes)} 的内容。大备份可能需要一些时间。</p>
  {:else if modal.kind==='restore'}<p>恢复“{modal.item.name}”：{modal.item.entries} 项、{bytes(modal.item.bytes)}。校验通过后才保存新目标，已有同名内容不会被覆盖。</p><label>目标存储目录<select aria-label="备份恢复目标存储目录" bind:value={toRoot} required disabled={pending}>{#each roots.filter((r)=>r.writable&&r.available) as root}<option value={root.id}>{root.label||root.id}</option>{/each}</select></label><label>新位置（相对目标存储目录）<input aria-label="备份恢复目标位置" bind:value={destination} maxlength="1024" required disabled={pending}/></label><p class="field-help">目标的上级文件夹需要已存在。停止时，已发布的目标会保留。</p>
  {:else}<p class="notice error">永久清理所选备份副本或不完整记录，释放其空间。原文件和已恢复的目标保留。取消只能保留尚未删除的部分。</p><p class="mono">{typeof modal.item==='string'?modal.item:modal.item.id}</p><label>输入完整版本编号确认<input aria-label="备份清理确认编号" bind:value={confirmation} required autocomplete="off" disabled={pending}/></label>{/if}
  {#if dialogError}<p class="notice error" role="alert">{dialogError}</p>{/if}
  <footer class="dialog-footer"><button class="button secondary" type="button" disabled={pending} onclick={close}>关闭</button><button class="button primary" type="submit" disabled={pending||readOnly||(modal.kind==='delete'||modal.kind==='cleanup')&&confirmation!==(typeof modal.item==='string'?modal.item:modal.item.id)}>{pending?'正在处理…':'确认'+name(modal.kind)}</button></footer>
</form></Dialog>{/if}
