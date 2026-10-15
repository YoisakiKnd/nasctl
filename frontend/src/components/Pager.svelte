<script>
  let { offset = 0, count = 0, pageSize = 50, busy = false, onchange } = $props();
</script>
<nav class="pager" aria-label="分页">
  <span class="muted">第 {Math.floor(offset / pageSize) + 1} 页</span>
  <div class="button-group">
    <button class="button secondary small" disabled={busy || offset === 0} onclick={() => onchange(Math.max(0, offset - pageSize))}>上一页</button>
    <button class="button secondary small" disabled={busy || count < pageSize} onclick={() => onchange(offset + pageSize)}>下一页</button>
  </div>
</nav>
