<script>
  import ErrorNotice from '../../components/ErrorNotice.svelte';
  import { onMount } from 'svelte';
  import { api } from '../../lib/api.js';
  import UserAccounts from '../../components/UserAccounts.svelte';
  let { auth = null, onaccountschange } = $props();
  let operations = $state([]), error = $state('');
  let disposed = false, request;
  onMount(() => {
    request = new AbortController();
    api('/api/v1/ops',{signal:request.signal}).then((data) => {if (!disposed) operations = data.operations || [];}).catch((failure) => {if (!disposed && failure.name !== 'AbortError') error = failure.message;});
    return () => {disposed = true; request?.abort();};
  });
</script>
<header class="page-heading"><div><h1>用户与权限</h1><p class="muted">管理员可管理全部设置；操作者可处理文件、相册和追番任务；只读用户只能浏览。</p></div></header>
{#if error}<ErrorNotice message={error}/>{/if}
<UserAccounts {auth} onchange={onaccountschange}/>
<details class="panel settings-section"><summary class="panel-heading">操作能力与限制<span class="muted">高级</span></summary><div class="table-wrap"><table><thead><tr><th>操作</th><th>权限级别</th><th>审批策略</th><th>超时</th></tr></thead><tbody>{#each operations as operation}<tr><td class="mono">{operation.id}</td><td>{operation.class}</td><td>{operation.approval}</td><td>{operation.timeout}</td></tr>{/each}</tbody></table></div></details>
