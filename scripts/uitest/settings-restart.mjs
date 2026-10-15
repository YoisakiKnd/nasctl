// Read-only verification after restarting the marked settings fixture.
const base=process.argv[2]||'http://127.0.0.1:18105';
const marker=await(await fetch(base+'/api/v1/files/download?root=data&path=.nasctl-ui-fixture')).text();
if(marker!=='nasctl-settings-fixture-v1\n')throw Error('generated settings fixture required');
const s=await(await fetch(base+'/api/v1/settings')).json();
const roots=await(await fetch(base+'/api/v1/files/roots')).json();
if(s.restart_required||s.active.storage[0].label!=='Saved generated files'||s.active.files.upload_max_bytes!==4096)throw Error('saved configuration not active after restart');
if(roots.upload_max_bytes!==4096||roots.roots[0].label!=='Saved generated files'||!roots.roots[0].capacity?.total_bytes)throw Error('file manager not using saved limits or real capacity');
if(JSON.stringify(s).includes('fixture-new-private-password'))throw Error('secret in restarted response');
const original=await(await fetch(base+'/api/v1/files/download?root=data&path=generated.txt')).text();
if(original!=='original settings fixture bytes\n')throw Error('original bytes changed');
console.log('Settings real restart passed: saved values active, restart hint cleared, real capacity and original bytes preserved.');
