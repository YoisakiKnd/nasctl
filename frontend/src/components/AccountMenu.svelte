<script>
  import { DropdownMenu } from 'bits-ui';
  import Icon from './Icon.svelte';
  // The header avatar opens the account menu: who is signed in, the account
  // page, the admin area, theme and sign-out. Phones also have the "我的" tab.
  let { displayName = '', roleLabel = '', theme = 'system', adminHref = '', canLogout = false, pending = false, onthemechange, onlogout } = $props();
  const themes = [{id:'system', label:'跟随系统'}, {id:'light', label:'浅色'}, {id:'dark', label:'深色'}];
  const go = (hash) => { window.location.hash = hash; };
</script>
<DropdownMenu.Root>
  <DropdownMenu.Trigger class="avatar" aria-label={'我的账号：' + displayName} title={displayName}>{(displayName || 'N').slice(0,1).toUpperCase()}</DropdownMenu.Trigger>
  <DropdownMenu.Portal>
    <DropdownMenu.Content class="menu" align="end" sideOffset={8}>
      <div class="menu-header"><strong>{displayName}</strong><small>{roleLabel}</small></div>
      <DropdownMenu.Separator class="menu-separator"/>
      <DropdownMenu.Item class="menu-item" onSelect={() => go('me')}><Icon name="user" size={16}/>账号与外观</DropdownMenu.Item>
      {#if adminHref}<DropdownMenu.Item class="menu-item" onSelect={() => go(adminHref)}><Icon name="settings" size={16}/>管理后台</DropdownMenu.Item>{/if}
      <DropdownMenu.Separator class="menu-separator"/>
      <DropdownMenu.RadioGroup value={theme} onValueChange={(value) => onthemechange?.(value)}>
        <DropdownMenu.GroupHeading class="menu-heading">主题</DropdownMenu.GroupHeading>
        {#each themes as item (item.id)}
          <DropdownMenu.RadioItem class="menu-item" value={item.id} closeOnSelect={false}>
            {#snippet children({ checked })}<span class="menu-check">{#if checked}<Icon name="check" size={15}/>{/if}</span>{item.label}{/snippet}
          </DropdownMenu.RadioItem>
        {/each}
      </DropdownMenu.RadioGroup>
      {#if canLogout}<DropdownMenu.Separator class="menu-separator"/><DropdownMenu.Item class="menu-item danger" disabled={pending} onSelect={() => onlogout?.()}><Icon name="back" size={16}/>退出登录</DropdownMenu.Item>{/if}
    </DropdownMenu.Content>
  </DropdownMenu.Portal>
</DropdownMenu.Root>
