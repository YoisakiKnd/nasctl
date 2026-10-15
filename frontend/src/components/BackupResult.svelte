<script>
  import { bytes } from '../lib/format.js';
  let { result, operation, state }=$props();
</script>
<section class="notice backup-result" aria-label="备份任务结果">
  {#if result.published}<strong>{operation==='backup.restore'?'恢复目标已保存':'备份版本已保存'}</strong>{:else if state==='completed'}<strong>{operation==='backup.verify'?'清单和全部文件校验通过':'清理已完成'}</strong>{:else}<strong>任务未全部完成</strong>{/if}
  {#if result.snapshot}<p>{result.snapshot.name} · {result.snapshot.entries} 项 · {bytes(result.snapshot.bytes)}</p><small class="mono">{result.snapshot.id}</small>{/if}
  {#if result.path}<p>恢复位置：{result.destination_root} / {result.path}</p>{/if}
  {#if result.pruned}<p>已清理 {result.pruned} 个备份版本。</p>{/if}
  {#if result.warning}<p class="inline-error">{result.warning}</p>{/if}
</section>
