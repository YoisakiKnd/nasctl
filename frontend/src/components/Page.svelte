<script>
  import { setContext } from 'svelte';
  // A visited page stays mounted while hidden so drafts, uploads and playback
  // survive navigation. Hidden pages skip polling through the visibility
  // context, and their dialogs stay inside the page instead of going modal.
  let { page, active, children } = $props();
  const dialogs = [];
  setContext('nasctl-window-visible', () => active);
  setContext('nasctl-window-dialogs', {
    register(close) { dialogs.push(close); return () => {const index = dialogs.indexOf(close); if (index >= 0) dialogs.splice(index, 1);}; },
    visible: () => active,
  });
</script>
<section class="page" class:page-fill={page.fill} class:active hidden={!active} data-page={page.id}>
  <div class="content" aria-label={page.title || page.label}>{@render children()}</div>
</section>
