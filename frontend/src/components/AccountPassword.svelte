<script>
  import { api,json } from '../lib/api.js';
  import Dialog from './Dialog.svelte';
  let { auth,oncomplete }=$props();
  let opened=$state(false),current=$state(''),password=$state(''),again=$state(''),pending=$state(false),error=$state(''),discard=$state(false);
  const valid=$derived(Array.from(password).length>=12&&new TextEncoder().encode(password).length<=1024&&password===again);
  function clear(){opened=false;current='';password='';again='';error='';discard=false;}
  function close(){if(pending)return;if(current||password||again){discard=true;return;}clear();}
  async function save(event){event.preventDefault();if(pending||!valid)return;pending=true;error='';try{await api('/api/v1/auth/password',json('POST',{current_password:current,password},{headers:{'X-Nasctl-Confirm':'1'}}));current='';password='';again='';opened=false;await oncomplete?.();}catch(e){error=e.message;}finally{pending=false;}}
</script>
<button class="avatar" aria-label="管理我的密码" title={(auth.display_name||auth.name)+' · 修改密码'} onclick={()=>{opened=true;discard=false;}}>{(auth.display_name||auth.name||'N').slice(0,1).toUpperCase()}</button>
{#if opened}<Dialog title="修改我的密码" onclose={close}>
  <p class="muted">{auth.display_name||auth.name} · {auth.name}。保存后，该账号的所有会话都需要重新登录。</p>
  <form onsubmit={save}><fieldset class="account-fields" disabled={pending}>
    <label>当前密码<input aria-label="我的当前密码" type="password" autocomplete="current-password" required maxlength="1024" bind:value={current}/></label>
    <label>新密码<input aria-label="我的新密码" type="password" autocomplete="new-password" required minlength="12" maxlength="1024" bind:value={password}/></label>
    <label>再次输入新密码<input aria-label="确认我的新密码" type="password" autocomplete="new-password" required maxlength="1024" bind:value={again}/></label>
    <p class="field-help">密码至少 12 个字符，最多 1024 字节。</p>
    {#if error}<p class="notice error" role="alert">{error}</p>{/if}
    <div class="dialog-footer"><button class="button secondary" type="button" onclick={close}>返回</button><button class="button primary" type="submit" disabled={!valid||!current}>{pending?'正在保存…':'确认修改并重新登录'}</button></div>
  </fieldset></form>
  {#if discard}<div class="notice"><p>密码尚未保存，关闭后会清空输入。</p><div class="button-row"><button class="button secondary" onclick={()=>discard=false}>继续编辑</button><button class="button danger" onclick={clear}>放弃修改</button></div></div>{/if}
</Dialog>{/if}
