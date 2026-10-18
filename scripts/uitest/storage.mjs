// Integration: the storage page. The fixture console has no root helper, so
// the page must explain how to enable it; the helper's answers are then
// replayed to check the drive cards, dialogs and the exact requests sent.
// Nothing reaches a real disk.
import {JSDOM,VirtualConsole} from 'jsdom';
import assert from 'node:assert/strict';
const base=process.argv[2]||'http://127.0.0.1:18090';
const token=process.argv[3]||'',headers=token?{Authorization:`Bearer ${token}`}:{};
const errors=[],vc=new VirtualConsole();vc.on('jsdomError',(error)=>errors.push(error.message));
const dom=new JSDOM(await(await fetch(base+'/',{headers})).text(),{url:base+'/',runScripts:'outside-only',pretendToBeVisual:true,virtualConsole:vc});
const w=dom.window,d=w.document,delay=(ms)=>new Promise((resolve)=>setTimeout(resolve,ms));
const check=(ok,label)=>{assert.ok(ok,label);console.log('ok:',label);};
async function wait(predicate,label){const until=Date.now()+8000;while(Date.now()<until){if(predicate())return;await delay(20);}throw new Error('timed out: '+label);}
const page=()=>d.querySelector('.page[data-page="admin/storage"]');
const button=(label,scope=d)=>[...(scope?.querySelectorAll('button')||[])].find((node)=>node.textContent.trim()===label);
const type=(node,value)=>{node.value=value;node.dispatchEvent(new w.Event('input',{bubbles:true}));};

let mock=null;const sent=[];
const disks=[
  {name:'mmcblk0',path:'/dev/mmcblk0',model:'',serial:'',transport:'mmc',size:15931539456,removable:false,read_only:false,system:true,partitions:[{name:'mmcblk0p1',path:'/dev/mmcblk0p1',size:15931539456,fstype:'ext4',label:'rootfs',uuid:'r',mountpoints:['/'],avail:9e9,used:5e9,system:true,managed:false}]},
  {name:'sda',path:'/dev/sda',model:'WD Elements',serial:'WX12345',transport:'usb',size:4000787030016,removable:false,read_only:false,system:false,partitions:[
    {name:'sda1',path:'/dev/sda1',size:4000785964544,fstype:'exfat',label:'Movies',uuid:'1234-ABCD',mountpoints:[],system:false,managed:false},
  ]},
  {name:'sdb',path:'/dev/sdb',model:'Samsung T7',serial:'S5T7',transport:'usb',size:1000204886016,removable:false,read_only:false,system:false,partitions:[
    {name:'sdb1',path:'/dev/sdb1',size:1000203837440,fstype:'ext4',label:'backup',uuid:'u-2',mountpoints:['/media/nasctl/backup'],avail:6e11,used:3e11,system:false,managed:true},
  ]},
];
const status={tools:{smbd:true,smartctl:true,'mount.cifs':true},automount:false,shares:[{name:'照片',path:'/media/nasctl/backup',writable:true,guest:false}],net_mounts:[{name:'office',kind:'smb',server:'10.0.0.5',share:'media',read_only:false,persist:true,mounted:true}],share_user:true,persisted:['/media/nasctl/backup']};
w.fetch=(path,init={})=>{
  const url=new URL(path,base);
  if(mock&&url.pathname.startsWith('/api/v1/storage')){
    const reply=(body,code=200)=>Promise.resolve(new Response(JSON.stringify(body),{status:code,headers:{'Content-Type':'application/json'}}));
    if(url.pathname==='/api/v1/storage')return reply({helper:true,disks,status});
    if(url.pathname==='/api/v1/storage/smart')return reply({available:true,passed:true,temperature_c:41,power_on_hours:9000,reallocated_sectors:0,pending_sectors:0});
    sent.push({path:url.pathname,body:JSON.parse(init.body),confirm:init.headers?.['X-Nasctl-Confirm']});
    return reply({ok:true});
  }
  return fetch(url,{...init,headers:{...init.headers,...headers}});
};
w.AbortController=AbortController;
w.matchMedia=()=>({matches:false,addEventListener(){},removeEventListener(){}});
w.HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','');};w.HTMLDialogElement.prototype.close=function(){this.removeAttribute('open');};
try{
  w.eval(await(await fetch(base+'/app.js',{headers})).text());
  await wait(()=>d.querySelector('.page.active'),'shell');
  w.location.hash='storage';await wait(()=>page()&&!page().hidden,'alias opens the storage page');
  check(d.querySelector('.sidebar-nav .nav-link.active')?.textContent.includes('存储与共享'),'storage sits in the settings navigation');
  await wait(()=>page().textContent.includes('启用存储助手'),'missing helper explained');
  check(page().querySelector('.command-block')?.textContent.includes('systemctl enable --now nasctl-storage.socket'),'the page shows how to enable the helper');

  mock=true;button('刷新',page()).click();await wait(()=>page().querySelectorAll('.drive-card').length===3,'drive cards');
  const [system,movies,backup]=page().querySelectorAll('.drive-card');
  check(system.textContent.includes('系统盘')&&!button('格式化',system)&&!button('挂载',system),'the system disk offers no mount or format');
  check(movies.querySelector('.drive-icon.usb')&&movies.textContent.includes('exFAT')&&movies.textContent.includes('WX12345'),'USB disk shows its file system and serial');
  check(backup.textContent.includes('/media/nasctl/backup')&&backup.textContent.includes('开机挂载')&&button('卸载',backup),'mounted disk shows where and offers unmount');
  check(page().textContent.includes('系统设置 › 存储位置'),'without /media/nasctl as a storage location the page says how to browse mounts');

  button('检查健康',movies).click();await wait(()=>movies.textContent.includes('41°C'),'SMART');
  check(movies.querySelector('.badge.complete')?.textContent==='健康'&&movies.textContent.includes('375 天'),'SMART summary shows health, temperature and power-on time');

  button('挂载',movies).click();await wait(()=>page().querySelector('dialog[open]'),'mount dialog');
  let dialog=page().querySelector('dialog[open]');
  check(dialog.querySelector('input').value==='Movies','mount name defaults to the label');
  check(!dialog.querySelector('input[role="switch"]').checked,'USB disks are not mounted at boot by default');
  button('挂载',dialog).click();await wait(()=>sent.length===1,'mount sent');
  check(sent[0].path==='/api/v1/storage/mount'&&sent[0].confirm==='1'&&sent[0].body.device==='/dev/sda1'&&sent[0].body.name==='Movies'&&sent[0].body.persist===false,'mount sends the device, name and confirmation');
  await wait(()=>!page().querySelector('dialog[open]')&&page().textContent.includes('已挂载到 /media/nasctl/Movies'),'mount confirmed');

  button('格式化',movies).click();await wait(()=>page().querySelector('dialog[open]'),'format dialog');
  dialog=page().querySelector('dialog[open]');const erase=button('永久删除并格式化',dialog);
  check(dialog.textContent.includes('所有数据都会被永久删除')&&erase.disabled,'format warns and starts disabled');
  type(dialog.querySelectorAll('input')[1],'sda');await delay(0);check(erase.disabled,'the device name is not enough when a serial exists');
  type(dialog.querySelectorAll('input')[1],'WX12345');await delay(0);check(!erase.disabled,'typing the serial enables formatting');
  button('取消',dialog).click();await wait(()=>!page().querySelector('dialog[open]'),'format cancelled');check(sent.length===1,'cancelling sends nothing');

  [...page().querySelectorAll('[role="tab"]')].find((node)=>node.textContent.startsWith('局域网共享')).click();await wait(()=>page().querySelector('.share-list'),'shares tab');
  check(page().textContent.includes('照片')&&page().textContent.includes('已设置密码'),'shares and the share account are listed');
  [...page().querySelectorAll('button')].find((node)=>node.textContent.trim()==='添加共享').click();dialog=await (async()=>{await wait(()=>page().querySelector('dialog[open]'),'share dialog');return page().querySelector('dialog[open]');})();
  type(dialog.querySelector('input'),'公开');dialog.querySelectorAll('input[role="switch"]')[1].click();await delay(0);
  button('保存',dialog).click();await wait(()=>sent.length===2,'shares sent');
  check(sent[1].path==='/api/v1/storage/shares'&&sent[1].body.shares.length===2&&sent[1].body.shares[1].name==='公开'&&sent[1].body.shares[1].guest===true&&sent[1].body.shares[1].path.startsWith('/'),'adding a share sends the whole list, defaulting to a storage location');

  [...page().querySelectorAll('[role="tab"]')].find((node)=>node.textContent.startsWith('网络存储')).click();await wait(()=>page().textContent.includes('//10.0.0.5/media'),'network tab');
  check(page().querySelector('.badge.complete')?.textContent==='已连接','network mount shows its state');
  check(errors.length===0,'no DOM errors: '+errors.join('; '));
  console.log('Storage page passed with replayed helper answers; no disk was touched.');
}finally{dom.window.close();}
