<script>
  import { getContext, onMount } from 'svelte';
  let { title, description = '', onclose, children, wide = false, photo = false, chromeHidden = false, onkey } = $props();
  let element;
  const id = $props.id();
  const host = getContext('nasctl-window-dialogs');
  onMount(() => {
    const previous = document.activeElement;
    const wasLocked = document.body.classList.contains('modal-open');
    const unregister = host?.register(() => onclose());
    if (host) element.setAttribute('open','');
    else {document.body.classList.add('modal-open'); element.showModal();}
    element.querySelector('input:not([type="hidden"]),select,textarea')?.focus();
    if (photo) element.querySelector('button')?.focus();
    return () => {
      const ownedFocus = element.contains(document.activeElement);
      unregister?.();
      if (!host) {element.close(); if (!wasLocked) document.body.classList.remove('modal-open');}
      if (ownedFocus && (!host || host.visible())) previous?.focus?.();
    };
  });
  function keys(event) {
    onkey?.(event);
    if (!host) return;
    if (event.key === 'Escape') {event.preventDefault(); event.stopPropagation(); onclose();}
    if (event.key !== 'Tab') return;
    const nodes = [...element.querySelectorAll('button:not(:disabled),a[href],input:not(:disabled):not([type="hidden"]),select:not(:disabled),textarea:not(:disabled),[tabindex="0"]')].filter((node)=>!node.hidden && !node.closest('[inert]'));
    if (!nodes.length) {event.preventDefault(); return;}
    if (event.shiftKey && document.activeElement===nodes[0]) {event.preventDefault(); nodes.at(-1).focus();}
    else if (!event.shiftKey && document.activeElement===nodes.at(-1)) {event.preventDefault(); nodes[0].focus();}
  }
</script>
<div class:window-modal-layer={host}>
<dialog aria-labelledby={id} bind:this={element} class:wide class:photo-dialog={photo} class:photo-immersive={chromeHidden} onkeydown={keys} oncancel={(event) => { event.preventDefault(); onclose(); }}>
  <div class="dialog-heading" inert={photo && chromeHidden}>
    <div class:sr-only={photo}><h2 {id}>{title}</h2>{#if description}<p class="muted">{description}</p>{/if}</div>
    <button class="icon-button" aria-label="关闭对话框" onclick={onclose}>{photo ? '‹' : '×'}</button>
  </div>
  {@render children()}
</dialog>
</div>
