<script>
  let { subscription } = $props();
  let failed = $state(false);
  $effect(() => { if (subscription.poster_available) failed = false; });
</script>
<div class="cover-placeholder" aria-hidden="true">
  {#if subscription.poster_available && !failed}<img src={`/api/v1/anime/subscriptions/${subscription.id}/poster?v=${encodeURIComponent(subscription.poster_version || '')}`} alt="" loading="lazy" decoding="async" onerror={() => failed = true}/>
  {:else}<strong class="cover-initial">{Array.from(subscription.title)[0]}</strong><span>S{String(subscription.season).padStart(2, '0')}</span>{/if}
</div>
