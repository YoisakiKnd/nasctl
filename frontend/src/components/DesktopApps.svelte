<script>
 import {onMount} from 'svelte';
 import {api,json} from '../lib/api.js';
 import {appDestination} from '../lib/app-links.js';
 import Icon from './Icon.svelte';
 import Dialog from './Dialog.svelte';
 let {query='',canConfigure=false,onchange=()=>{}}=$props();
 let data=$state({apps:[],revision:''}),error=$state(''),editing=$state(false),draft=$state(null),pending=$state(false),dialogError=$state('');
 let request,disposed=false;
 const shown=$derived(data.apps.filter(a=>(a.name+' '+a.url).toLowerCase().includes(query.trim().toLowerCase())));
 const destination=value=>appDestination(value,window.location.hostname);
 async function load(){request?.abort();request=new AbortController();try{const response=await api('/api/v1/apps',{signal:request.signal});if(!disposed){data=response;onchange(data.apps);error='';}}catch(e){if(!disposed&&e.name!=='AbortError')error=e.message;}}
 function edit(app=null){draft=app?{...app}:{id:'app-'+Date.now().toString(36),name:'',url:window.location.protocol+'//{host}:8096/',icon:'external',color:'blue'};editing=true;dialogError='';}
 async function save(event,remove=false){event?.preventDefault();if(pending||!canConfigure)return;let apps=data.apps.filter(a=>a.id!==draft.id);if(!remove){const old=data.apps.findIndex(a=>a.id===draft.id);if(old>=0)apps.splice(old,0,{...draft});else apps.push({...draft});}pending=true;dialogError='';try{data=await api('/api/v1/apps',json('PUT',{apps,revision:data.revision},{headers:{'X-Nasctl-Confirm':'1'}}));onchange(data.apps);draft=null;}catch(e){dialogError=e.message;}finally{pending=false;}}
 onMount(()=>{load();return()=>{disposed=true;request?.abort();};});
</script>
{#each shown as app(app.id)}<a class={'desktop-app custom-app '+app.color} href={destination(app.url)} target="_blank" rel="noopener noreferrer"><span class="app-icon"><Icon name={app.icon} size={29}/></span><strong>{app.name}</strong><small>打开应用 <Icon name="external" size={10}/></small></a>{/each}
{#if canConfigure && !query.trim()}<button class="desktop-app custom-app-add" onclick={()=>{editing=true;draft=null;}} aria-label="自定义桌面应用"><span class="app-icon"><Icon name="plus" size={27}/></span><strong>添加应用</strong><small>连接你的其他服务</small></button>{/if}
{#if error}<p class="notice error custom-app-error">{error}<button class="text-button" onclick={load}>重试</button></p>{/if}
{#if editing}<Dialog title={draft?'应用快捷方式':'我的应用快捷方式'} onclose={()=>{if(!pending){editing=false;draft=null;}}}>
 {#if draft}<form class="shortcut-form" onsubmit={save}><label>应用名称<input aria-label="自定义应用名称" bind:value={draft.name} required maxlength="64" placeholder="Jellyfin"/></label><label>应用地址<input aria-label="自定义应用地址" bind:value={draft.url} required maxlength="2048" placeholder={'http://{host}:8096/'}/></label><p class="field-help">用 {'{host}'} 代表 NAS 地址，例如 http://{'{host}'}:8096/。只创建跳转入口，应用需要已在 NAS 上运行。快捷方式对 NAS 成员可见，请使用不含账号密码或访问令牌的地址。</p><div class="form-grid"><label>图标<select aria-label="自定义应用图标" bind:value={draft.icon}>{#each [['external','应用'],['play','播放器'],['anime','媒体'],['cloud','云服务'],['folder','文件'],['album','照片'],['disk','存储'],['home','主页'],['jobs','任务'],['backup','备份']] as [value,label]}<option {value}>{label}</option>{/each}</select></label><label>颜色<select aria-label="自定义应用颜色" bind:value={draft.color}>{#each [['blue','蓝色'],['teal','青色'],['rose','粉色'],['violet','紫色'],['amber','橙色'],['slate','灰色']] as [value,label]}<option {value}>{label}</option>{/each}</select></label></div>{#if dialogError}<p class="notice error" role="alert">{dialogError}</p>{/if}<footer class="dialog-footer">{#if data.apps.some(a=>a.id===draft.id)}<button type="button" class="text-button danger-text" disabled={pending} onclick={()=>save(null,true)}>移除快捷方式</button>{/if}<button type="button" class="button secondary" disabled={pending} onclick={()=>draft=null}>返回</button><button class="button primary" disabled={pending} type="submit">{pending?'正在保存…':'保存应用'}</button></footer></form>
 {:else}<p class="muted">把播放器、下载器等常用服务放到桌面，一点就打开。</p><div class="shortcut-list">{#each data.apps as app(app.id)}<button onclick={()=>edit(app)}><span class={'app-icon '+app.color}><Icon name={app.icon} size={20}/></span><span><strong>{app.name}</strong><small>{app.url}</small></span><Icon name="edit" size={17}/></button>{/each}</div><footer class="dialog-footer"><button class="button secondary" onclick={()=>editing=false}>完成</button><button class="button primary" disabled={data.apps.length>=32} onclick={()=>edit()}>添加快捷方式</button></footer>{/if}
</Dialog>{/if}
