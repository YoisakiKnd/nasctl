<script>
  import { onDestroy } from 'svelte';
  import { api } from '../lib/api.js';
  import Icon from './Icon.svelte';
  let { disabled=false, onselect, onbusy }=$props();
  let provider=$state('mikan'), query=$state(''), results=$state(null), busy=$state(false), error=$state(''), page=$state(1);
  let bangumi=$state(null), source=$state(null), feed=$state(''), applied=$state(false);
  let request, generation=0;
  const mismatch=$derived(Boolean(bangumi&&source?.bangumi_id&&bangumi.id!==source.bangumi_id));
  function stop(){++generation;request?.abort();busy=false;onbusy?.(false);}
  onDestroy(stop);
  function switchProvider(value){stop();provider=value;results=null;source=null;feed='';error='';bangumi=null;page=1;}
  async function run(path, apply){
    stop();const current=++generation;request=new AbortController();busy=true;onbusy?.(true);error='';
    try{const result=await api(path,{signal:request.signal});if(current===generation)apply(result);}
    catch(failure){if(current===generation&&failure.name!=='AbortError')error=failure.message;}
    finally{if(current===generation){busy=false;onbusy?.(false);}}
  }
  function search(number=1){if(!query.trim()||disabled)return;source=null;feed='';page=number;run('/api/v1/anime/sources/search?'+new URLSearchParams({provider,q:query.trim(),page:number}),value=>results=value);}
  function choose(item){
    if(provider==='bangumi'){bangumi=item;provider='mikan';query=item.name;results=null;search();}
    else{source=null;feed='';run('/api/v1/anime/sources/mikan/'+item.id,value=>source=value);}
  }
  function apply(){const chosen=source?.feeds.find(item=>String(item.id)===feed);if(!chosen||mismatch)return;onselect({title:bangumi?.name||source.name,feed_url:chosen.url,include:'.+'});applied=true;}
</script>
<section class="anime-source-picker" aria-label="搜索番剧添加">
  <div class="source-picker-heading"><div><h2>找到想追的那一部</h2><p class="muted">搜索番剧，选择字幕组，再添加到你的追番。</p></div><Icon name="search" size={23}/></div>
  {#if applied}<div class="source-picked-title"><strong>{source.name}</strong><span>{source.feeds.find(item=>String(item.id)===feed)?.name}</span><button type="button" class="text-button" disabled={disabled} onclick={()=>applied=false}>重新选择来源</button></div>{:else}
  <div class="segmented" aria-label="番剧搜索来源"><button type="button" class:active={provider==='mikan'} disabled={disabled||busy} onclick={()=>switchProvider('mikan')}>蜜柑 Mikan</button><button type="button" class:active={provider==='bangumi'} disabled={disabled||busy} onclick={()=>switchProvider('bangumi')}>Bangumi</button></div>
  {#if bangumi}<p class="source-picked-title"><strong>{bangumi.name}</strong><span>接下来选择 Mikan 对应条目与字幕组</span><button type="button" class="text-button" disabled={disabled||busy} onclick={()=>switchProvider('bangumi')}>重新选番</button></p>{/if}
  <div class="source-search-row"><input aria-label="搜索 Bangumi 或 Mikan 番剧" bind:value={query} maxlength="100" placeholder="输入中文或日文番名…" disabled={disabled||busy} onkeydown={event=>{if(event.key==='Enter'){event.preventDefault();search();}}}/><button type="button" class="button primary" disabled={disabled||busy||!query.trim()} onclick={()=>search()}>{busy?'搜索中…':'搜索番剧'}</button></div>
  <p class="field-help">Bangumi 提供番剧资料；下载订阅来自 Mikan。搜索不会添加订阅或开始下载。</p>
  {#if error}<p class="notice error" role="alert">{error}</p>{/if}
  {#if results&&!source}<div class="source-search-results" aria-label="番剧搜索结果">{#each results.results as item(item.id)}<button type="button" class="source-result" data-source-id={item.id} disabled={disabled||busy} onclick={()=>choose(item)}>{#if item.poster}<img src={item.poster} alt="" loading="lazy" referrerpolicy="no-referrer"/>{:else}<span class="source-poster-placeholder"><Icon name="anime" size={25}/></span>{/if}<span><strong>{item.name}</strong>{#if item.original_name&&item.original_name!==item.name}<small>{item.original_name}</small>{/if}{#if item.date}<small>{item.date}</small>{/if}<small>{provider==='bangumi'?'选择番剧，查找 Mikan 源':'选择字幕组与 RSS'}</small></span><Icon name="chevron" size={16}/></button>{/each}</div>
    {#if !results.results.length}<p class="notice">没有找到匹配番剧，试试日文原名、简称或另一来源，也可以在下方手动填写 RSS。</p>{/if}
    {#if provider==='bangumi'&&(page>1||results.has_more)}<div class="button-group"><button type="button" class="text-button" disabled={busy||disabled||page<=1} onclick={()=>search(page-1)}>上一页</button><span class="muted">第 {page} 页</span><button type="button" class="text-button" disabled={busy||disabled||!results.has_more} onclick={()=>search(page+1)}>下一页</button></div>{/if}
    {#if results.truncated}<p class="field-help">仅显示前 12 个匹配结果，请输入更完整的名称缩小范围。</p>{/if}
  {/if}
  {#if source}<section class="source-feed-choice" aria-label="选择 Mikan 字幕组"><div class="section-bar"><h3>{source.name}</h3><a class="text-button" href={source.url} target="_blank" rel="noopener noreferrer">查看来源 ↗</a></div>
    {#if mismatch}<p class="notice error" role="alert">该 Mikan 条目关联的 Bangumi 编号与所选番剧不同，请返回选择对应的季。</p>{:else if bangumi}<p class="field-help">{source.bangumi_id?'与所选 Bangumi 条目一致。':'来源未提供 Bangumi 编号，请核对名称和季数。'}</p>{/if}
    <label>字幕组<select aria-label="Mikan 字幕组" bind:value={feed} disabled={disabled||busy||mismatch}><option value="" disabled>选择字幕组</option>{#each source.feeds as item}<option value={String(item.id)}>{item.name}</option>{/each}</select></label>
    <p class="field-help">同一番剧建议选一个字幕组。下方可进一步筛选清晰度与语言；季数请按媒体库的季数确认。</p>
    {#if source.truncated}<p class="field-help">字幕组过多，仅显示前 64 个；其他源可手动填写 RSS。</p>{/if}
    <div class="button-group"><button type="button" class="button primary" disabled={disabled||busy||!feed||mismatch} onclick={apply}>使用这个订阅源</button><button type="button" class="text-button" disabled={disabled||busy} onclick={()=>{source=null;feed='';}}>返回搜索结果</button></div>
  </section>{/if}
  {/if}
</section>
