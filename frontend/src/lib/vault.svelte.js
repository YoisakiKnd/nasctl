import { api, json } from './api.js';

// One vault session per browser tab, shared by the album, the file browser
// and the admin pages. The server locks it after 15 idle minutes; this module
// mirrors that, locks when the tab has been hidden for two minutes, and tells
// the server when the tab closes.
const confirm = {'X-Nasctl-Confirm':'1'};
const hiddenGrace = 2 * 60_000;

// open asks the album to show its vault tab, e.g. from the account page.
export const vault = $state({status:null, pending:false, open:false});

export function openVault() {vault.open = true; window.location.hash = 'album';}

let expiry, hiddenTimer, poll, request;

function schedule() {
  clearTimeout(expiry); clearInterval(poll);
  if (!vault.status?.unlocked) return;
  expiry = setTimeout(() => refreshVault(), Math.max(1000, vault.status.expires_at * 1000 - Date.now() + 500));
  // The server extends an idle session on every private read; poll so the
  // countdown follows it without extending it ourselves.
  poll = setInterval(() => {if (!document.hidden) refreshVault();}, 60_000);
}

function settle(status) {
  const was = vault.status?.unlocked;
  vault.status = status;
  schedule();
  if (was && !status.unlocked) window.dispatchEvent(new CustomEvent('nasctl:vault-locked'));
  if (!was && status.unlocked) window.dispatchEvent(new CustomEvent('nasctl:vault-unlocked'));
  return status;
}

export async function refreshVault() {
  request?.abort(); request = new AbortController();
  try {return settle(await api('/api/v1/album/vault/status', {signal:request.signal}));}
  catch (failure) {if (failure.name !== 'AbortError' && failure.status === 403) settle({available:false}); return vault.status;}
}

async function call(operation, body = {}) {
  vault.pending = true;
  try {return settle(await api('/api/v1/album/vault/' + operation, json('POST', body, {headers:confirm, timeoutMs:50_000})));}
  finally {vault.pending = false;}
}

export const createVault = (password) => call('create', {password});
export const unlockVault = (password) => call('unlock', {password});
export const changeVaultPassword = (password, next) => call('password', {password, new_password:next});

export async function lockVault() {
  if (vault.status?.unlocked) settle({...vault.status, unlocked:false, expires_at:0});
  try {settle(await api('/api/v1/album/vault/lock', {method:'POST', headers:confirm}));} catch {}
}

// A private request answered "locked" means the session ended elsewhere.
export function vaultLockedResponse(failure) {
  if (failure?.code !== 'vault_locked') return false;
  if (vault.status?.unlocked) settle({...vault.status, unlocked:false, expires_at:0});
  return true;
}

export function startVault() {
  refreshVault();
  const visibility = () => {
    clearTimeout(hiddenTimer);
    if (document.hidden && vault.status?.unlocked) hiddenTimer = setTimeout(lockVault, hiddenGrace);
    // The cookie is shared by every tab, so coming back may find the vault
    // unlocked or locked from another tab.
    else if (!document.hidden) refreshVault();
  };
  const leaving = () => {
    if (!vault.status?.unlocked) return;
    // Best effort; the server's idle expiry still applies if this is lost.
    fetch('/api/v1/album/vault/lock', {method:'POST', headers:confirm, credentials:'same-origin', keepalive:true}).catch(() => {});
  };
  document.addEventListener('visibilitychange', visibility);
  window.addEventListener('pagehide', leaving);
  return () => {
    clearTimeout(expiry); clearTimeout(hiddenTimer); clearInterval(poll); request?.abort();
    document.removeEventListener('visibilitychange', visibility); window.removeEventListener('pagehide', leaving);
  };
}

export function remaining(status, now = Date.now()) {
  const seconds = Math.max(0, Math.round((status?.expires_at || 0) - now / 1000));
  return seconds >= 60 ? Math.ceil(seconds / 60) + ' 分钟' : seconds + ' 秒';
}
