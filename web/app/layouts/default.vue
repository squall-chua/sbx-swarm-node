<script setup lang="ts">
import type { NavigationMenuItem } from '@nuxt/ui'

const session = useSession()

const nodeName = ref('')

onMounted(async () => {
  try {
    // loadRole() fetches GET /v1/node, sets session.role, and returns the node info —
    // reuse that single fetch for the node name (no second request, no internals reach).
    const info = await session.loadRole()
    nodeName.value = info?.node_name ?? ''
  } catch {
    // Not yet authenticated or network error — don't crash
  }
})

const roleBadgeColor = computed(() => {
  if (session.role.value === 'admin') return 'primary'
  return 'neutral'
})

const navItems = computed<NavigationMenuItem[]>(() => [
  { label: 'Overview', icon: 'i-lucide-layout-dashboard', to: '/' },
  { label: 'Sandboxes', icon: 'i-lucide-box', to: '/sandboxes' },
  { label: 'Nodes', icon: 'i-lucide-server', to: '/nodes' },
  { label: 'Templates', icon: 'i-lucide-file-code', to: '/templates' },
  { label: 'Network', icon: 'i-lucide-network', to: '/network' },
  { label: 'Operations', icon: 'i-lucide-activity', to: '/operations' },
  { label: 'Settings', icon: 'i-lucide-settings', to: '/settings' },
])
</script>

<template>
  <UDashboardGroup>
    <UDashboardSidebar collapsible resizable>
      <template #header="{ collapsed }">
        <div
          class="flex items-center gap-2 px-2 py-1"
          :class="collapsed ? 'justify-center' : ''"
        >
          <UIcon name="i-lucide-zap" class="size-5 text-primary shrink-0" />
          <span v-if="!collapsed" class="font-semibold text-sm text-highlighted truncate">
            Swarm Console
          </span>
        </div>
      </template>

      <template #default="{ collapsed }">
        <UNavigationMenu
          :collapsed="collapsed"
          :items="navItems"
          orientation="vertical"
          class="px-1"
        />
      </template>

      <template #footer="{ collapsed }">
        <UButton
          :icon="collapsed ? 'i-lucide-log-out' : undefined"
          :label="collapsed ? undefined : 'Sign out'"
          :aria-label="collapsed ? 'Sign out' : undefined"
          color="neutral"
          variant="ghost"
          block
          @click="session.logout()"
        />
      </template>
    </UDashboardSidebar>

    <!-- Shared panel wrapper: provides the top navbar across all authenticated pages -->
    <UDashboardPanel>
      <template #header>
        <UDashboardNavbar>
          <template #leading>
            <UDashboardSidebarCollapse />
          </template>

          <template #left>
            <span
              v-if="nodeName"
              class="font-mono text-sm text-muted"
              aria-label="Node name"
            >{{ nodeName }}</span>
          </template>

          <template #right>
            <UBadge
              v-if="session.role.value"
              :color="roleBadgeColor"
              variant="subtle"
              class="capitalize"
            >
              {{ session.role.value }}
            </UBadge>
            <UButton
              icon="i-lucide-log-out"
              aria-label="Sign out"
              color="neutral"
              variant="ghost"
              size="sm"
              @click="session.logout()"
            />
          </template>
        </UDashboardNavbar>
      </template>

      <template #body>
        <slot />
      </template>
    </UDashboardPanel>
  </UDashboardGroup>
</template>
