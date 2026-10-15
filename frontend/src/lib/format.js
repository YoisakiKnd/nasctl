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
export const jobLabels = { queued: '排队中', running: '运行中', waiting: '等待中', paused: '已暂停', completed: '已完成', failed: '失败', interrupted: '已中断', unknown: '状态未知', cancelled: '已取消' };
