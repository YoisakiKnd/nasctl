<script>
  import { Collapsible } from 'bits-ui';
  import Icon from './Icon.svelte';
  // An error message whose first line is the summary. Anything after it is a
  // technical detail (which step failed, the HTTP status, the server's reply)
  // shown only when the reader expands it, with a copy button for reporting.
  let { message = '', onretry, inline = false, alert = true, children } = $props();
  const text = $derived(String(message ?? ''));
  const summary = $derived(text.split('\n')[0]);
  const detail = $derived(text.includes('\n') ? text.slice(text.indexOf('\n') + 1).trim() : '');
  let open = $state(false), copied = $state(false), pre = $state();
  async function copy() {
    const value = summary + '\n' + detail;
    try { await navigator.clipboard.writeText(value); copied = true; }
    catch {
      // Plain-HTTP pages have no clipboard API; select the text instead.
      const range = document.createRange(); range.selectNodeContents(pre);
      const selection = window.getSelection(); selection?.removeAllRanges(); selection?.addRange(range);
      copied = document.execCommand?.('copy') ?? false;
    }
    if (copied) setTimeout(() => copied = false, 2000);
  }
</script>
{#if text}
<div class={inline ? 'inline-error error-notice' : 'notice error error-notice'} role={alert && !inline ? 'alert' : undefined}>
  {#if !inline}<Icon name="warning" size={17}/>{/if}
  <div class="error-body">
    <p>{summary}</p>
    {#if detail}
      <Collapsible.Root bind:open>
        <Collapsible.Trigger class="error-toggle">{open ? '收起详情' : '查看详情'}<Icon name="chevron" size={13} class={open ? 'turned' : ''}/></Collapsible.Trigger>
        <Collapsible.Content class="error-detail">
          <pre bind:this={pre}>{detail}</pre>
          <button type="button" class="text-button" onclick={copy}>{copied ? '已复制' : '复制错误信息'}</button>
        </Collapsible.Content>
      </Collapsible.Root>
    {/if}
    {@render children?.()}
  </div>
  {#if onretry}<button type="button" class="text-button" onclick={onretry}>重试</button>{/if}
</div>
{/if}
