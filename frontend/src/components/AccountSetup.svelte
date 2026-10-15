<script>
  import { api,json } from '../lib/api.js';
  let { oncomplete, embedded=false }=$props();
  let code=$state(''),username=$state(''),display=$state(''),password=$state(''),again=$state(''),confirmed=$state(false),pending=$state(false),error=$state('');
  const validPassword=$derived(Array.from(password).length>=12&&new TextEncoder().encode(password).length<=1024);
  async function submit(event){event.preventDefault();if(pending||!confirmed||password!==again||!validPassword)return;pending=true;error='';try{await api('/api/v1/auth/setup',json('POST',{code,username,display_name:display,password},{headers:{'X-Nasctl-Confirm':'1'}}));code='';password='';again='';await oncomplete?.();}catch(e){error=e.message;if(['audit_failed','accounts_changed'].includes(e.code))await oncomplete?.();}finally{pending=false;}}
</script>
<section class="account-setup" class:embedded>
  <h2>设置你的 NAS</h2><p class="muted">创建第一个管理员账号，再添加存储目录和媒体库。</p>
  <p class="field-help">首次设置密钥只保存在 NAS 本机。请在设备终端运行 <code>nasctl users setup-code --config 配置文件路径</code> 读取它；不要把密钥发给别人。</p>
  <form onsubmit={submit}><fieldset disabled={pending}>
    <label>首次设置密钥<input aria-label="首次设置密钥" type="password" autocomplete="off" maxlength="43" required bind:value={code}/></label>
    <div class="settings-form-grid"><label>管理员用户名<input aria-label="管理员用户名" autocomplete="username" pattern={'[a-z0-9][a-z0-9_.\\-]{0,31}'} maxlength="32" required bind:value={username}/><small class="field-help">小写字母、数字、点、下划线或短横线。</small></label><label>显示名称<input aria-label="管理员显示名称" autocomplete="nickname" maxlength="40" bind:value={display}/></label></div>
    <label>新密码<input aria-label="管理员新密码" type="password" autocomplete="new-password" minlength="12" maxlength="1024" required bind:value={password}/></label>
    <label>再次输入密码<input aria-label="确认管理员密码" type="password" autocomplete="new-password" minlength="12" maxlength="1024" required bind:value={again}/></label>
    {#if again&&password!==again}<p class="field-help error-text">两次密码不一致。</p>{/if}
    <label class="checkbox"><input type="checkbox" bind:checked={confirmed}/>创建管理员并启用账号登录。此账号可以管理所有已配置的文件、媒体与设备设置。</label>
    {#if error}<p class="notice error" role="alert">{error}</p>{/if}
    <button class="button primary" type="submit" disabled={pending||!confirmed||password!==again||!validPassword||!username||!code}>{pending?'正在创建管理员…':'创建管理员，进入 NAS'}</button>
  </fieldset></form>
</section>
