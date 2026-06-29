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
  { label: 'Workspaces', icon: 'i-lucide-folder-git-2', to: '/workspaces' },
  { label: 'Network', icon: 'i-lucide-network', to: '/network' },
  { label: 'Operations', icon: 'i-lucide-activity', to: '/operations' },
  { label: 'Settings', icon: 'i-lucide-settings', to: '/settings' },
])

// ── Command palette (⌘K / Ctrl-K) ─────────────────────────────────────────────
const swarm = useSwarm()
const status = useStatus()
const paletteOpen = ref(false)

defineShortcuts({ meta_k: () => { paletteOpen.value = true } })

function go(to: string) {
  paletteOpen.value = false
  navigateTo(to)
}

const paletteGroups = computed(() => [
  {
    id: 'pages',
    label: 'Pages',
    items: navItems.value.map(n => ({ label: n.label as string, icon: n.icon as string, onSelect: () => go(n.to as string) })),
  },
  {
    id: 'sandboxes',
    label: 'Sandboxes',
    items: (swarm?.sandboxes.value ?? []).map((sb: any) => ({
      label: sb.id,
      icon: status.sandbox(sb.status).icon,
      suffix: sb.status,
      onSelect: () => go(`/sandboxes?id=${encodeURIComponent(sb.id)}`),
    })),
  },
  {
    id: 'nodes',
    label: 'Nodes',
    items: (swarm?.nodes.value ?? []).map((n: any) => ({
      label: n.node_name,
      icon: 'i-lucide-server',
      suffix: n.node_id,
      onSelect: () => go('/nodes'),
    })),
  },
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
            <UButton
              color="neutral"
              variant="outline"
              size="sm"
              icon="i-lucide-search"
              aria-label="Open command palette"
              @click="paletteOpen = true"
            >
              <span class="hidden sm:inline text-muted">Search</span>
              <template #trailing>
                <div class="hidden sm:flex items-center gap-0.5">
                  <UKbd value="meta" />
                  <UKbd value="k" />
                </div>
              </template>
            </UButton>
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

    <!-- Command palette (⌘K) — jump to any node, sandbox, or page -->
    <UModal v-model:open="paletteOpen" :ui="{ content: 'sm:max-w-xl' }">
      <template #content>
        <UCommandPalette
          :groups="paletteGroups"
          placeholder="Search nodes, sandboxes, pages…"
          close
          class="h-80"
          @update:open="paletteOpen = $event"
        />
      </template>
    </UModal>
  </UDashboardGroup>
</template>
