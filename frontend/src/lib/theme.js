// Theme is 'system', 'light' or 'dark'. Without a saved choice the stylesheet
// follows prefers-color-scheme by itself, so the first paint never flashes.
// data-theme always reflects the effective theme for components that need it.
const key = 'nasctl-theme';
let media, listener;

function effective(choice) {
  if (choice === 'light' || choice === 'dark') return choice;
  return media?.matches ? 'dark' : 'light';
}

export function applyTheme(choice) {
  media ||= window.matchMedia?.('(prefers-color-scheme: dark)') || null;
  if (media && listener) media.removeEventListener?.('change', listener);
  listener = null;
  try { if (choice === 'light' || choice === 'dark') localStorage.setItem(key, choice); else localStorage.removeItem(key); } catch { /* storage can be disabled */ }
  document.documentElement.dataset.theme = effective(choice);
  if (choice !== 'light' && choice !== 'dark' && media) {
    listener = () => { document.documentElement.dataset.theme = effective('system'); };
    media.addEventListener?.('change', listener);
  }
}

export function loadTheme() {
  let choice = 'system';
  try { const saved = localStorage.getItem(key); if (saved === 'light' || saved === 'dark') choice = saved; } catch { /* storage can be disabled */ }
  applyTheme(choice);
  return choice;
}
