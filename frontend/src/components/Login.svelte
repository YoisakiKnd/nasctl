<script>
  import ErrorNotice from './ErrorNotice.svelte';
  import { api,json } from '../lib/api.js';
  import Icon from './Icon.svelte';
  import AccountSetup from './AccountSetup.svelte';
  let { available=true,accounts=false,setup=false,tokenAvailable=true,onlogin }=$props();
  let token=$state(''),username=$state(''),password=$state(''),useToken=$state(false),remember=$state(false),pending=$state(false),error=$state('');
  const accountMode=$derived(accounts&&!useToken);
  async function login(event){event.preventDefault();if(pending||(accountMode?!username||!password:!token))return;pending=true;error='';try{await api('/api/v1/auth/login',json('POST',accountMode?{username,password,remember}:{token}));token='';password='';await onlogin();}catch(e){error=e.message;}finally{pending=false;}}
</script>
<main class="login-page"><section class="login-card panel" aria-labelledby="login-title"><span class="brand-symbol"><Icon name="disk" size={25}/></span><p class="eyebrow">nasctl · 个人存储控制台</p>
  {#if setup}<h1 id="login-title">欢迎来到你的空间</h1><AccountSetup oncomplete={onlogin}/>
  {:else}<h1 id="login-title">回到你的空间</h1>
    {#if available}<p class="muted">{accountMode?'使用你的 NAS 账号登录。':'输入设备的访问令牌，管理文件与媒体库。'}</p><form onsubmit={login}>
      {#if accountMode}<label for="account-username">用户名</label><input id="account-username" name="username" autocomplete="username" maxlength="32" required bind:value={username} disabled={pending}/><label for="account-password">密码</label><input id="account-password" name="password" type="password" autocomplete="current-password" maxlength="1024" required bind:value={password} disabled={pending}/><label class="check-row login-remember"><input type="checkbox" bind:checked={remember} disabled={pending}/><span>保持登录 30 天<small>仅在自己的设备上勾选</small></span></label>
      {:else}<label for="access-token">访问令牌</label><input id="access-token" name="token" type="password" autocomplete="current-password" required bind:value={token} disabled={pending}/>{/if}
      {#if error}<ErrorNotice message={error}/>{/if}<button class="button primary" type="submit" disabled={pending||(accountMode?!username||!password:!token)}>{pending?'正在登录…':'进入控制台'}</button>
    </form>{#if accounts&&tokenAvailable}<button class="text-button login-alternative" disabled={pending} onclick={()=>{useToken=!useToken;error='';password='';token='';}}>{useToken?'使用 NAS 账号登录':'使用设备访问令牌'}</button>{/if}<p class="field-help">{accountMode&&remember?'30 天内无需重新登录，服务重启也保持；修改密码或退出登录会立即失效。':'登录有效期为 12 小时，服务重启后需重新登录。'}忘记密码时，请由设备操作者在 NAS 本机重置。</p>
    {:else}<p class="notice warning">此设备尚未提供可用的网页登录方式。请在 NAS 本机检查账号或访问令牌配置。</p>{/if}
  {/if}
</section></main>
