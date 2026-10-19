export function bytes(value) {
  if (value === undefined || value === null || !Number.isFinite(value)) return '—';
  if (value === 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  const i = Math.min(Math.max(Math.floor(Math.log(Math.max(value, 1)) / Math.log(1024)), 0), units.length - 1);
  return `${(value / 1024 ** i).toLocaleString('zh-CN', { maximumFractionDigits: i ? 1 : 0 })} ${units[i]}`;
}

export function time(value) {
  if (!value) return '尚未刷新';
  const date = new Date(typeof value === 'number' ? value * 1000 : value);
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
}

export function duration(value) {
  const seconds = Math.max(0, Math.floor(Number(value) || 0));
  const minutes = Math.floor(seconds / 60), remainder = String(seconds % 60).padStart(2,'0');
  return minutes >= 60 ? `${Math.floor(minutes/60)}:${String(minutes%60).padStart(2,'0')}:${remainder}` : `${minutes}:${remainder}`;
}

export const episodeLabels = { discovered: '待选择', queued: '已排队', submitted: '已提交', downloading: '下载中', paused: '下载已暂停', checking: '校验中', archived: '已入库', failed: '失败', ignored: '已忽略' };
export const modeLabels = { normal: '正常运行', maintenance: '维护模式', watching: '观影模式', manual_pause: '已暂停', resource_guard: '资源保护' };
// Readable names for the operations a person meets in task panels.
const opNames = {
  'files.upload': '上传文件', 'files.edit': '编辑文本', 'files.mkdir': '新建文件夹', 'files.rename': '移动或重命名',
  'files.copy': '复制', 'files.move': '移动', 'files.extract': '解压', 'files.batch': '批量整理', 'files.purge': '永久删除',
  'files.trash': '移入回收站', 'files.restore': '恢复文件',
  'backup.run': '本地备份', 'backup.verify': '校验备份', 'backup.restore': '恢复本地备份', 'backup.delete': '删除备份', 'backup.cleanup': '清理备份',
  'backup.webdav.run': '上传备份', 'backup.webdav.restore': '恢复备份', 'backup.webdav.retry': '重试上传',
  'album.scan': '扫描相册', 'album.thumbs': '生成缩略图', 'album.vault.move': '移入保险箱',
  'anime.refresh': '刷新订阅', 'anime.sync': '同步下载', 'anime.metadata.refresh': '更新番剧信息',
  'anime.history.cleanup': '清理下载记录', 'anime.history.export': '导出下载记录',
};
export const opLabel = (op) => opNames[op] || op;

// File errors arrive from background tasks as the server's own sentence;
// the summary is shown in Chinese and the original kept as the detail.
const fileErrors = [
  ['budget reached', '达到条目、深度或总大小上限，请拆分后重试'],
  ['source changed', '源内容在处理期间发生了变化，请核对后重试'],
  ['destination already exists', '目标名称已存在，已有内容未被覆盖'],
  ['not a readable ZIP', '这不是可读取的 ZIP 压缩包，或者文件已损坏'],
  ['encrypted or uses an unsupported', '压缩包已加密或使用了不支持的压缩方式，请下载后用本地软件解压'],
  ['malformed path', '包含无效或不安全的路径（例如 ../）'],
  ['outside the storage root', '包含存储目录外或内部保留的路径'],
  ['insufficient space', '存储空间或配额不足'],
  ['storage is read-only', '此存储目录未启用写入'],
  ['storage root is unavailable', '存储目录不可用，请检查挂载'],
];
export function jobError(text) {
  const match = fileErrors.find(([key]) => text?.includes(key));
  return match ? match[1] + '\n' + text : text;
}

export const jobLabels = { queued: '排队中', running: '运行中', waiting: '等待中', paused: '已暂停', completed: '已完成', failed: '失败', interrupted: '已中断', unknown: '状态未知', cancelled: '已取消' };
