// Desktop preferences, kept per browser: which wallpaper, which widgets, and
// where the search box sends a query. The custom wallpaper image itself is
// stored on the NAS and shared by every member.
const KEY = 'nasctl-desktop-v1';

export const wallpapers = [
  {id:'sky', label:'晴空', css:'radial-gradient(900px 500px at 12% -10%,#3fb0f0 0%,transparent 60%),radial-gradient(800px 600px at 100% 110%,#1c5fa8 0%,transparent 55%),linear-gradient(150deg,#0a8fd8 0%,#0076bb 45%,#0a5c9c 100%)'},
  {id:'mountain', label:'雪山', css:'radial-gradient(1200px 500px at 50% 115%,#0b1a2e 0%,transparent 70%),linear-gradient(170deg,#7aa7d6 0%,#3d6fa8 38%,#1e3f6e 70%,#0f2140 100%)'},
  {id:'aurora', label:'极光', css:'radial-gradient(900px 420px at 20% 10%,#3ee6b8aa 0%,transparent 60%),radial-gradient(800px 520px at 85% 20%,#7b5cff99 0%,transparent 60%),linear-gradient(160deg,#0b2a3a 0%,#0e1a33 60%,#070d1c 100%)'},
  {id:'dusk', label:'暮色', css:'radial-gradient(900px 500px at 80% 0%,#ffb27a 0%,transparent 55%),radial-gradient(900px 600px at 10% 100%,#5b3c8f 0%,transparent 60%),linear-gradient(160deg,#e07a6a 0%,#8a4a8f 50%,#2e2a5a 100%)'},
  {id:'forest', label:'森林', css:'radial-gradient(900px 500px at 15% 0%,#9bd27a99 0%,transparent 60%),linear-gradient(160deg,#2f6b4f 0%,#1d4a3a 50%,#0f2a22 100%)'},
  {id:'graphite', label:'石墨', css:'radial-gradient(900px 500px at 20% 0%,#5a6474 0%,transparent 60%),linear-gradient(160deg,#3a404a 0%,#23272e 55%,#14161a 100%)'},
];

export const engines = [
  {id:'files', label:'NAS 文件'},
  {id:'baidu', label:'百度', url:'https://www.baidu.com/s?wd='},
  {id:'bing', label:'必应', url:'https://www.bing.com/search?q='},
  {id:'google', label:'Google', url:'https://www.google.com/search?q='},
];

export const widgetList = [
  {id:'clock', label:'时钟'},
  {id:'status', label:'系统状态'},
  {id:'storage', label:'存储'},
  {id:'network', label:'网络'},
];

function load() {
  const base = {wallpaper:'sky', engine:'files', widgets:{clock:true, status:true, storage:true, network:true}, revision:0};
  try {
    const saved = JSON.parse(localStorage.getItem(KEY) || 'null');
    if (saved && typeof saved === 'object') {
      return {...base, ...saved, widgets:{...base.widgets, ...(saved.widgets || {})}, custom:false};
    }
  } catch { /* A browser without storage keeps the defaults. */ }
  return {...base, custom:false};
}

export const desktop = $state(load());

export function saveDesktop() {
  try {
    const {wallpaper, engine, widgets, revision} = desktop;
    localStorage.setItem(KEY, JSON.stringify({wallpaper, engine, widgets, revision}));
  } catch { /* Preferences are a convenience. */ }
}

// The CSS background for the chosen wallpaper. A custom choice falls back to
// the default until the NAS confirms it has one.
export function wallpaperCSS() {
  if (desktop.wallpaper === 'custom' && desktop.custom) {
    return `center / cover no-repeat url("/api/v1/desktop/wallpaper?v=${desktop.revision}"), ${wallpapers[0].css}`;
  }
  return (wallpapers.find((item) => item.id === desktop.wallpaper) || wallpapers[0]).css;
}

export async function checkCustomWallpaper() {
  try {
    const response = await fetch('/api/v1/desktop/wallpaper?exists=1', {credentials:'same-origin'});
    if (!response.ok) return;
    const custom = (await response.json()).custom === true;
    if (desktop.custom !== custom) desktop.custom = custom;
  } catch { /* Keep the last known state. */ }
}

// Large photos are scaled down in the browser before upload: the NAS never
// decodes an image inside a web request.
export async function uploadWallpaper(file) {
  const bitmap = await createImageBitmap(file);
  const edge = 2560, scale = Math.min(1, edge / Math.max(bitmap.width, bitmap.height));
  const canvas = document.createElement('canvas');
  canvas.width = Math.round(bitmap.width * scale); canvas.height = Math.round(bitmap.height * scale);
  canvas.getContext('2d').drawImage(bitmap, 0, 0, canvas.width, canvas.height);
  bitmap.close?.();
  const blob = await new Promise((resolve) => canvas.toBlob(resolve, 'image/jpeg', 0.85));
  if (!blob) throw new Error('图片无法读取，请换一张 JPEG 或 PNG 图片');
  const response = await fetch('/api/v1/desktop/wallpaper', {method:'PUT', credentials:'same-origin', headers:{'Content-Type':'image/jpeg', 'X-Nasctl-Confirm':'1'}, body:blob});
  if (!response.ok) throw new Error((await response.json().catch(() => null))?.message || '壁纸上传失败');
  desktop.custom = true; desktop.wallpaper = 'custom'; desktop.revision = Date.now(); saveDesktop();
}

export async function removeWallpaper() {
  const response = await fetch('/api/v1/desktop/wallpaper', {method:'DELETE', credentials:'same-origin', headers:{'X-Nasctl-Confirm':'1'}});
  if (!response.ok) throw new Error((await response.json().catch(() => null))?.message || '壁纸删除失败');
  desktop.custom = false; if (desktop.wallpaper === 'custom') desktop.wallpaper = 'sky'; saveDesktop();
}
