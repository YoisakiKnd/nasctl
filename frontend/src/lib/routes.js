// Hash routes. User pages are for everyday browsing and watching; admin pages
// configure the device and run its work. Old desktop-window hashes stay valid.

export const userPages = [
  {id:'home', label:'首页', icon:'home'},
  {id:'files', label:'文件', title:'文件管理', icon:'folder', fill:true},
  {id:'album', label:'相册', icon:'album', fill:true},
  {id:'anime', label:'追番', icon:'anime'},
  {id:'me', label:'我的', icon:'user', mobileOnly:true},
];

// roles: which account roles may open the page. Admin means a device manager.
export const adminPages = [
  {id:'admin', label:'概览', icon:'overview', group:'', roles:['admin','operator']},
  {id:'admin/jobs', label:'任务中心', icon:'jobs', group:'', roles:['admin','operator']},
  {id:'admin/anime', label:'追番管理', icon:'anime', group:'内容', roles:['admin','operator']},
  {id:'admin/album', label:'相册管理', icon:'album', group:'内容', roles:['admin','operator']},
  {id:'admin/storage', label:'存储与共享', icon:'disk', group:'数据', roles:['admin']},
  {id:'admin/backup', label:'备份', icon:'backup', group:'数据', roles:['admin']},
  {id:'admin/users', label:'用户与权限', icon:'users', group:'系统', roles:['admin']},
  {id:'admin/system', label:'系统设置', icon:'settings', group:'系统', roles:['admin']},
  {id:'admin/logs', label:'操作记录', icon:'list', group:'系统', roles:['admin']},
];

const aliases = {
  '': 'home', overview: 'admin', media: 'anime', jobs: 'admin/jobs',
  settings: 'admin/system', backup: 'admin/backup', storage: 'admin/storage',
};

export function roleOf(auth) {
  if (auth?.role === 'viewer') return 'viewer';
  if (auth?.role === 'operator') return 'operator';
  return 'admin';
}

export function allowedAdminPages(auth) {
  const role = roleOf(auth);
  // A non-manager principal such as a helper token sees operator pages only.
  return adminPages.filter((page) => page.roles.includes(role) && (page.roles.includes('operator') || auth?.can_manage_device !== false));
}

// resolve maps a hash to a page the account may open, falling back to home.
export function resolve(hash, auth) {
  let id = hash.replace(/^#\/?/, '').replace(/\/+$/, '');
  id = aliases[id] ?? id;
  if (id === 'admin' && roleOf(auth) === 'viewer') return 'home';
  if (userPages.some((page) => page.id === id)) return id;
  if (allowedAdminPages(auth).some((page) => page.id === id)) return id;
  return 'home';
}

export function pageInfo(id) {
  return userPages.find((page) => page.id === id) || adminPages.find((page) => page.id === id);
}
