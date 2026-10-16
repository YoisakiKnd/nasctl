// Runs the real compiled Svelte bundle in jsdom against an isolated Go fixture.
// Browser screenshots separately verify layout and breakpoints.
import { JSDOM, VirtualConsole } from 'jsdom';
const base = process.argv[2] || 'http://127.0.0.1:18090';
const token = process.argv[3] || '';
const headers = token ? {Authorization:`Bearer ${token}`} : {};
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
function check(value, label) {if (!value) throw new Error(label); console.log('ok:',label);}
async function wait(predicate, label, timeout = 6000) {
  const until = Date.now() + timeout;
  while (Date.now() < until) {if (predicate()) return; await pause(30);}
  throw new Error('timed out: ' + label);
}
const response = await fetch(base + '/', {headers});
check(response.ok,'application shell responds');
const logs = [];
const consoleOutput = new VirtualConsole();
consoleOutput.on('jsdomError',(error) => logs.push(error.message));
const dom = new JSDOM(await response.text(),{url:base+'/#overview',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:consoleOutput});
const w = dom.window, d = w.document;
let mockPreview = false, previewBody = null;
w.fetch = (path, init = {}) => {
  // Preview rendering uses a controlled response; backend tests separately fetch
  // a fixture RSS through the engine. Production never has a private-feed bypass.
  if (mockPreview && new URL(path,base).pathname === '/api/v1/anime/preview') {
    previewBody = JSON.parse(init.body);
    return Promise.resolve(new Response(JSON.stringify({scanned:2,matched:1,new:1,existing:0,truncated:false,items:[{title:'Fixture - 05',number:5,matched:true,reason:'可发现的新剧集'},{title:'Fixture - 02',number:2,matched:false,reason:'不在集数范围内'}]})));
  }
  return fetch(new URL(path,base),{...init,headers:{...init.headers,...headers}});
};
w.AbortController = AbortController;
w.matchMedia = () => ({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal = function() {this.setAttribute('open','');};
w.HTMLDialogElement.prototype.close = function() {this.removeAttribute('open'); this.dispatchEvent(new w.Event('close'));};
w.confirm = () => {throw new Error('unexpected confirmation dialog');};
const text = (selector) => d.querySelector(selector)?.textContent || '';
const click = (selector) => {const node = d.querySelector(selector); check(node,'control exists: '+selector); node.click();};
const input = (selector,value) => {const node = d.querySelector(selector); check(node,'input exists: '+selector); node.value = value; node.dispatchEvent(new w.Event('input',{bubbles:true}));};
async function route(view) {
  w.location.hash = view;
  await wait(() => d.querySelector('.page.active .content')?.getAttribute('aria-label') === ({overview:'概览',files:'文件管理',media:'追番','admin/anime':'追番管理',jobs:'任务中心',settings:'系统设置',album:'相册',backup:'备份'})[view],'route '+view);
  await pause(80);
}
try {
  const js = await fetch(base+'/app.js',{headers}); check(js.ok,'compiled Svelte bundle responds');
  w.eval(await js.text());
  await wait(() => text('#overview-stamp').includes('更新于'),'overview resolves');
  check(d.querySelectorAll('.sidebar-nav .nav-link').length === 8,'all eight admin pages are listed for a device manager');
  check(!text('.system-status').includes('正在连接'),'global status resolves');
  await route('files');
  await wait(() => d.querySelectorAll('.file-name').length > 0,'file listing resolves');
  const firstDir = d.querySelector('.entry-dir');
  check(firstDir,'fixture has a directory'); firstDir.dispatchEvent(new w.MouseEvent('dblclick',{bubbles:true}));
  await wait(() => d.querySelectorAll('.breadcrumbs button').length >= 2,'directory navigation updates breadcrumbs');
  click('.breadcrumbs button');
  await wait(() => d.querySelectorAll('.breadcrumbs button').length === 1,'root navigation works');
  input('input[aria-label="搜索文件"]','<img');
  d.querySelector('.finder-search').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
  await pause(0);
  await wait(() => !d.querySelector('.loading-state') && [...d.querySelectorAll('.entry-label')].some((node) => node.textContent.includes('<img')),'hostile filename is searchable');
  check(d.querySelectorAll('.file-name img,.file-name script').length === 0,'hostile filename renders as text, never markup');
  d.querySelector('.file-name').click(); await wait(()=>d.querySelector('.finder-details a[download]'),'selected download');
  check(d.querySelector('a[download]')?.getAttribute('href').includes('/files/download?'),'files have a dedicated download link');
  const download = await fetch(new URL(d.querySelector('a[download]').getAttribute('href'),base),{headers});
  check(download.ok,'file download URL works against the real API');
  await download.arrayBuffer();
  d.querySelector('.file-name').dispatchEvent(new w.MouseEvent('dblclick',{bubbles:true}));
  await wait(() => d.querySelector('dialog[open]'),'file preview opens');
  await wait(() => !text('dialog').includes('正在准备预览'),'file stat resolves');
  check(!d.querySelector('dialog .notice.error'),'file stat path is accepted');
  click('dialog button[aria-label="关闭对话框"]');
  await wait(() => !d.querySelector('dialog'),'file preview closes');
  await route('media');
  await wait(() => !d.querySelector('.page.active .loading-state'),'anime status resolves');
  check(![...d.querySelectorAll('.page.active button')].some((node) => node.textContent.includes('添加订阅')),'the watching page has no subscription management');
  await route('admin/anime');
  await wait(() => !d.querySelector('.page.active .loading-state'),'anime management resolves');
  const add = [...d.querySelectorAll('button')].find((node) => node.textContent.includes('添加订阅') && !node.disabled);
  if (add) {
    const fixtureName = 'UI Harness ' + Date.now();
    add.click(); await wait(() => d.querySelector('dialog[open]'),'subscription dialog opens');
    input('input[name="title"]',fixtureName);
    input('input[name="feed_url"]','https://example.com/fixture.rss');
    check(!d.querySelector('input[name="auto_download"]').checked,'auto download is off by default');
    input('input[name="start_episode"]','5');
    input('input[name="end_episode"]','12');
    await pause(0); // Flush Svelte's reactive disabled state before clicking.
    mockPreview = true;
    [...d.querySelectorAll('dialog button')].find((node) => node.textContent.includes('预览 RSS 匹配')).click();
    await wait(() => d.querySelector('.feed-preview'),'RSS preview renders');
    check(previewBody.start_episode === 5 && previewBody.end_episode === 12,'preview evaluates the unsaved episode range');
    check(text('.feed-preview').includes('不在集数范围内'),'preview explains rejected releases');
    mockPreview = false;
    d.querySelector('dialog form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
    await wait(() => !d.querySelector('dialog') && text('.page.active .subscription-grid').includes(fixtureName),'subscription is saved through the real API');
    const subscriptionResponse = await fetch(base + '/api/v1/anime/subscriptions',{headers});
    const saved = (await subscriptionResponse.json()).subscriptions.find((sub) => sub.title === fixtureName);
    check(saved.start_episode === 5 && saved.end_episode === 12,'episode range is persisted by the real API');
    check(d.querySelector('.page.active .stat-strip'),'subscription management shows statistics');
    const card = [...d.querySelectorAll('.subscription-card')].find((node) => node.textContent.includes(fixtureName));
    [...card.querySelectorAll('button')].find((node) => node.textContent.includes('编辑')).click();
    await wait(() => d.querySelector('dialog[open]'),'edit dialog opens');
    d.querySelector('input[name="enabled"]').click();
    d.querySelector('dialog form').dispatchEvent(new w.Event('submit',{bubbles:true,cancelable:true}));
    await wait(() => !d.querySelector('dialog') && [...d.querySelectorAll('.subscription-card')].some((node) => node.textContent.includes(fixtureName) && node.textContent.includes('已暂停')),'subscription can be paused');
  } else check(text('.page.active .empty-state').includes('尚未启用'),'disabled anime module is explained');
  await route('jobs'); await wait(() => !d.querySelector('.loading-state'),'jobs resolve');
  await route('settings'); await wait(() => !text('.panel-heading .badge').includes('读取中'),'settings resolve');
  check(d.querySelector('.mode-options'),'mode controls are preserved');
  await route('album'); check(d.querySelector('.album-view'),'integrated album entry is preserved');
  await route('backup');
  await wait(()=>d.querySelector('.cloud-backup'),'WebDAV backup is the default view');
  [...d.querySelectorAll('.page.active button')].find(node=>node.textContent.trim()==='本地副本').click();
  await wait(()=>d.querySelector('.backup-plans')&&d.querySelector('.backup-history'),'native backup view'); check(!text('.page.active .content').includes('尚未接入'),'native backup plan/history replaces placeholder');
  const before = d.documentElement.dataset.theme;
  click('.theme-toggle');
  await pause(50);
  check(before !== d.documentElement.dataset.theme,'theme toggle works');
  check(logs.length === 0,'no jsdom errors: '+logs.join('; '));
  console.log('All Svelte DOM flows passed. Layout still requires real-browser verification.');
} finally {dom.window.close();}
