// Centralized status vocabularies for the swarm. There are TWO distinct ones —
// merging them is wrong, because the same word means different things:
//   - operation lifecycle: "running" = in-progress (warning), "done" = success
//   - sandbox/resource state: "running" = healthy (success), "done" = success
// Each entry carries color + icon + label so <StatusPill> can render dot+icon+
// text (status is never conveyed by color alone — accessibility).

export type StatusColor = 'success' | 'warning' | 'error' | 'info' | 'neutral'

export interface StatusMeta {
  color: StatusColor
  icon: string
  label: string
}

const SANDBOX: Record<string, StatusMeta> = {
  running: { color: 'success', icon: 'i-lucide-circle-play', label: 'Running' },
  published: { color: 'success', icon: 'i-lucide-circle-check', label: 'Published' },
  done: { color: 'success', icon: 'i-lucide-circle-check', label: 'Done' },
  pending: { color: 'warning', icon: 'i-lucide-loader', label: 'Pending' },
  'running-operation': { color: 'warning', icon: 'i-lucide-loader-circle', label: 'Operation running' },
  draining: { color: 'warning', icon: 'i-lucide-droplet', label: 'Draining' },
  stopped: { color: 'error', icon: 'i-lucide-circle-stop', label: 'Stopped' },
  deleted: { color: 'error', icon: 'i-lucide-trash-2', label: 'Deleted' },
  lost: { color: 'error', icon: 'i-lucide-circle-help', label: 'Lost' },
  error: { color: 'error', icon: 'i-lucide-circle-alert', label: 'Error' },
  publish_failed: { color: 'error', icon: 'i-lucide-circle-x', label: 'Publish failed' },
  revoke: { color: 'error', icon: 'i-lucide-ban', label: 'Revoked' },
}

const OPERATION: Record<string, StatusMeta> = {
  pending: { color: 'warning', icon: 'i-lucide-loader', label: 'Pending' },
  running: { color: 'warning', icon: 'i-lucide-loader-circle', label: 'Running' },
  done: { color: 'success', icon: 'i-lucide-circle-check', label: 'Done' },
  error: { color: 'error', icon: 'i-lucide-circle-alert', label: 'Error' },
  failed: { color: 'error', icon: 'i-lucide-circle-x', label: 'Failed' },
}

function meta(map: Record<string, StatusMeta>, status: string): StatusMeta {
  // Unknown status: neutral, keep the raw value as the label.
  return map[status] ?? { color: 'neutral', icon: 'i-lucide-circle-dashed', label: status }
}

export function useStatus() {
  return {
    sandbox: (status: string): StatusMeta => meta(SANDBOX, status),
    operation: (status: string): StatusMeta => meta(OPERATION, status),
  }
}
