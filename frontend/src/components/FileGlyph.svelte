<script>
  let { entry, size=56 } = $props();
  const extension=$derived((entry.name?.split('.').at(-1)||'').slice(0,5).toUpperCase());
  const color=$derived(({image:'#7b67d8',video:'#e48664',audio:'#cb6d9b',text:'#6d92ad',archive:'#bc9651',directory:'#439be8'})[entry.kind] || '#7f91a5');
</script>
<svg width={size} height={size} viewBox="0 0 64 64" fill="none" aria-hidden="true" class="file-glyph">
  {#if entry.is_dir}
    <path d="M5 16a5 5 0 0 1 5-5h14l6 6h24a5 5 0 0 1 5 5v29a5 5 0 0 1-5 5H10a5 5 0 0 1-5-5Z" fill="#6ab5ee"/>
    <path d="M5 25a5 5 0 0 1 5-5h44a5 5 0 0 1 5 5v26a5 5 0 0 1-5 5H10a5 5 0 0 1-5-5Z" fill="#3c9ae7"/>
    <path d="M9 24h46" stroke="#a8d8fa" stroke-width="1.5" stroke-linecap="round"/>
  {:else}
    <path d="M15 5h23l12 13v37a4 4 0 0 1-4 4H15a4 4 0 0 1-4-4V9a4 4 0 0 1 4-4Z" fill="var(--file-paper,#f9fbff)" stroke="var(--file-paper-border,#cbd4df)"/>
    <path d="M38 5v13h12" fill="var(--file-fold,#e7edf5)" stroke="var(--file-paper-border,#cbd4df)" stroke-linejoin="round"/>
    {#if entry.kind==='image'}<path d="m18 37 7-8 7 7 5-5 8 10H18Z" fill={color} opacity=".8"/><circle cx="35" cy="25" r="3" fill={color}/>
    {:else if entry.kind==='video'}<path d="m26 25 14 8-14 8Z" fill={color}/>
    {:else if entry.kind==='audio'}<path d="M29 36V23l12-3v13M29 26l12-3" stroke={color} stroke-width="3"/><ellipse cx="25" cy="37" rx="5" ry="4" fill={color}/><ellipse cx="37" cy="34" rx="5" ry="4" fill={color}/>
    {:else}<path d="M20 28h21M20 34h21M20 40h13" stroke={color} stroke-width="2" stroke-linecap="round"/>{/if}
    <rect x="15" y="46" width="31" height="12" rx="3" fill={color}/><text x="30.5" y="54.5" text-anchor="middle" fill="white" font-family="system-ui,sans-serif" font-size="7" font-weight="700">{extension || 'FILE'}</text>
  {/if}
</svg>
