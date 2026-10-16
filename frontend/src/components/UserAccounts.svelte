<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api,json } from '../lib/api.js';
  import { time } from '../lib/format.js';
  import Dialog from './Dialog.svelte';
  import AccountSetup from './AccountSetup.svelte';
  let { auth=null,onchange }=$props();
  let snapshot=$state(null),loading=$state(false),pending=$state(false),error=$state(''),message=$state(''),dialog=$state(null),username=$state(''),display=$state(''),role=$state('viewer'),disabled=$state(false),password=$state(''),again=$state(''),confirmName=$state(''),discard=$state(false);
  let disposed=false,request;
  const dirty=$derived(dialog?.kind==='edit'&&(username!==(dialog.user?.username||'')||display!==(dialog.user?.display_name||'')||role!==(dialog.user?.role||'viewer')||disabled!==(dialog.user?.disabled||false)||password||again));
  function clearDialog(){dialog=null;password='';again='';discard=false;}
  function closeDialog(){if(pending)return;if(dirty){discard=true;return;}clearDialog();}
  const labels={admin:'管理员',operator:'操作者',viewer:'只读用户'};
  const validPassword=$derived(!password||Array.from(password).length>=12&&new TextEncoder().encode(password).length<=1024);
  async function load(){if(loading||pending)return;loading=true;error='';request?.abort();request=new AbortController();try{const data=await api('/api/v1/users',{signal:request.signal});if(!disposed)snapshot=data;}catch(e){if(!disposed&&e.name!=='AbortError')error=e.message;}finally{if(!disposed)loading=false;}}
  function edit(user=null){discard=false;dialog={kind:'edit',user};username=user?.username||'';display=user?.display_name||'';role=user?.role||'viewer';disabled=user?.disabled||false;password='';again='';confirmName='';error='';message='';}
  function remove(user){discard=false;dialog={kind:'remove',user};confirmName='';error='';message='';}
  async function save(){if(pending||!dialog||!snapshot)return;pending=true;error='';const current=dialog;try{const options={headers:{'X-Nasctl-Confirm':'1'}};let data;
    if(current.kind==='remove'){data=await api('/api/v1/users/'+encodeURIComponent(current.user.id),json('DELETE',{revision:snapshot.revision,confirm_username:confirmName},options));}
    else{data=await api('/api/v1/users',json('POST',{revision:snapshot.revision,id:current.user?.id||'',username,display_name:display,role,disabled,password},options));}
    if(disposed)return;snapshot=data;dialog=null;password='';again='';message=current.kind==='remove'?'账号已删除，会话已撤销。':'账号已保存。该账号原有会话需要重新登录。';await onchange?.();
  }catch(e){if(!disposed)error=e.message;}finally{if(!disposed)pending=false;}}
  async function setupComplete(){await onchange?.();await load();}
  onMount(()=>{load();return()=>{disposed=true;request?.abort();};});
</script>
<section class="panel settings-section" aria-label="账号与权限">
  <div class="panel-heading"><div><h2>账号与权限</h2><p class="muted">为设备成员设置登录身份和操作范围。</p></div><button class="button secondary small" disabled={loading||pending} onclick={load}>刷新账号</button></div>
  {#if error&&!dialog}<ErrorNotice message={error}/>{/if}{#if message}<p class="notice success" role="status">{message}</p>{/if}
  {#if snapshot?.setup_requires_config}<div class="notice"><strong>先启用持久登录保护</strong><p>在 NAS 本机的基础配置中设置 <code>[hub] setup=true</code>，然后重启服务。这样即使状态盘丢失，设备也会要求重新验证管理员。</p><p>重启后，在本机运行 <code>nasctl users setup-code</code> 获取首次设置密钥。</p></div>
  {:else if snapshot&&!snapshot.enabled}<AccountSetup embedded oncomplete={setupComplete}/>
  {:else if snapshot}
    <div class="account-list-body"><p class="field-help">这些账号共享已配置的文件和媒体库。管理员管理设备与授权；操作者整理文件和媒体任务；只读用户浏览与播放。账号角色不划分独立文件夹。</p>
      <div class="panel-heading compact"><span class="muted">{snapshot.users.length} / {snapshot.max_users} 个账号</span><button class="button primary small" disabled={pending||snapshot.users.length>=snapshot.max_users} onclick={()=>edit()}>添加账号</button></div>
      <ul class="account-list">{#each snapshot.users as user (user.id)}<li><div><strong>{user.display_name}</strong><span class="muted">{user.username}{user.id===auth?.user_id?' · 当前账号':''}</span><small class="muted">创建于 {time(user.created_at)}</small></div><span class="badge" class:failed={user.disabled}>{user.disabled?'已停用':labels[user.role]}</span><div class="account-actions"><button class="text-button" disabled={pending} onclick={()=>edit(user)}>编辑</button><button class="text-button danger-text" disabled={pending} onclick={()=>remove(user)}>删除</button></div></li>{/each}</ul>
    </div>
  {:else if loading}<p class="section-empty" role="status">正在读取账号…</p>{/if}
</section>
{#if dialog}<Dialog title={dialog.kind==='remove'?'删除账号':dialog.user?'编辑账号':'添加账号'} onclose={closeDialog}>
  {#if dialog.kind==='remove'}<p>删除“{dialog.user.username}”并撤销现有会话，原文件保留。</p><label class="dialog-label">输入完整用户名确认<input aria-label="确认删除账号用户名" autocomplete="off" bind:value={confirmName} disabled={pending}/></label><div class="dialog-footer"><button class="button secondary" disabled={pending} onclick={closeDialog}>返回</button><button class="button danger" disabled={pending||confirmName!==dialog.user.username} onclick={save}>{pending?'正在删除…':'确认删除账号'}</button></div>
  {:else}<form onsubmit={(e)=>{e.preventDefault();save();}}><fieldset class="account-fields" disabled={pending}>
    <div class="settings-form-grid"><label>用户名<input aria-label="账号用户名" autocomplete="username" maxlength="32" pattern={'[a-z0-9][a-z0-9_.\\-]{0,31}'} required readonly={Boolean(dialog.user)} bind:value={username}/></label><label>显示名称<input aria-label="账号显示名称" autocomplete="off" maxlength="40" bind:value={display}/></label></div>
    <label>角色<select aria-label="账号角色" bind:value={role}><option value="viewer">只读用户 · 浏览与播放</option><option value="operator">操作者 · 整理文件与媒体任务</option><option value="admin">管理员 · 设备、账号与授权</option></select></label>
    <label class="checkbox"><input type="checkbox" bind:checked={disabled}/>停用此账号</label>
    <div class="settings-form-grid"><label>{dialog.user?'新密码（留空保留）':'新密码'}<input aria-label="账号新密码" type="password" autocomplete="new-password" minlength="12" maxlength="1024" required={!dialog.user} bind:value={password}/></label><label>再次输入新密码<input aria-label="确认账号新密码" type="password" autocomplete="new-password" maxlength="1024" required={Boolean(password)} bind:value={again}/></label></div>
    {#if !validPassword}<p class="field-help error-text">密码至少 12 个字符，且不超过 1024 字节。</p>{/if}{#if again&&again!==password}<p class="field-help error-text">两次密码不一致。</p>{/if}
    <p class="field-help">保存角色、停用状态或密码后，该账号已有会话会失效；至少保留一个已启用的管理员。</p>
    <div class="dialog-footer"><button class="button secondary" type="button" onclick={closeDialog}>返回</button><button class="button primary" type="submit" disabled={pending||!username||(!dialog.user&&!password)||!validPassword||password!==again}>{pending?'正在保存…':'确认保存账号'}</button></div>
  </fieldset></form>{/if}
  {#if error}<ErrorNotice message={error}/>{/if}
  {#if discard}<div class="notice"><p>账号修改尚未保存，关闭后会放弃当前输入。</p><div class="button-row"><button class="button secondary" onclick={()=>discard=false}>继续编辑</button><button class="button danger" onclick={clearDialog}>放弃修改</button></div></div>{/if}
</Dialog>{/if}
