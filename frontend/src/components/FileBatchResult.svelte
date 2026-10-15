<script>
  import { bytes } from '../lib/format.js';
  let { result }=$props();
  const codes={exists:'目标同名，未覆盖',not_found:'项目或上级目录不存在',budget_exhausted:'达到整批预算',interrupted:'已停止或超时',read_only:'只读目录',no_space:'空间不足',busy:'另有写入进行中',trash_full:'回收站已满',source_changed:'源内容变化',outside_root:'路径超出根目录',root_unavailable:'存储不可用',failed:'操作未完成'};
</script>
<section class="file-batch-result" aria-label="批量操作结果">
  <p>完成 {result.completed} 项 · 失败 {result.failed} 项 · 未开始 {result.not_started} 项 · 已处理 {result.entries} 个条目、{bytes(result.bytes)}</p>
  <div class="table-wrap"><table><thead><tr><th>项目</th><th>结果</th><th>位置／回收记录</th></tr></thead><tbody>{#each result.items as item}<tr><td>{item.root} / {item.path||item.id}</td><td>{item.state==='completed'?'完成':item.state==='not_started'?'未开始':'失败'}{item.code?' · '+(codes[item.code]||item.code):''}{item.published&&item.state!=='completed'?' · 目标已保存，请核对源文件':''}</td><td>{item.destination||item.trash_id||'—'}</td></tr>{/each}</tbody></table></div>
</section>
