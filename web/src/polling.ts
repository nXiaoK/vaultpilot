export function pollWhileVisible(refresh: () => Promise<void>, interval: number, immediate = true): () => void {
  let timer: ReturnType<typeof setTimeout> | undefined
  let stopped = false
  let running = false
  let refreshAgain = false
  async function run() {
    if (stopped || document.hidden) return
    if (running) { refreshAgain = true; return }
    refreshAgain = false
    running = true
    try { await refresh() }
    finally {
      running = false
      if (!stopped && !document.hidden) {
        timer = setTimeout(run, refreshAgain ? 0 : interval)
        refreshAgain = false
      }
    }
  }
  function visibilityChanged() {
    if (timer) clearTimeout(timer)
    if (!document.hidden) void run()
  }
  document.addEventListener('visibilitychange', visibilityChanged)
  if (!document.hidden) {
    if (immediate) void run()
    else timer = setTimeout(run, interval)
  }
  return () => { stopped = true; if (timer) clearTimeout(timer); document.removeEventListener('visibilitychange', visibilityChanged) }
}
