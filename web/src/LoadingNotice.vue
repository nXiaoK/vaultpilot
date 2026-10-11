<script setup lang="ts">
import { LoaderCircle } from 'lucide-vue-next'
defineProps<{message: string}>()
</script>

<template>
  <div class="loading-notice" role="status" aria-live="polite" aria-atomic="true">
    <LoaderCircle class="spin" :size="20" aria-hidden="true"/>
    <div><strong>{{message}}</strong><small>请求处理中，完成后会显示结果。</small></div>
    <span class="loading-dots" aria-hidden="true"><i/><i/><i/></span>
  </div>
</template>

<style scoped>
.loading-notice{display:flex;align-items:center;gap:12px;background:#f0f6f1;border:1px solid #d8e6db;border-radius:8px;padding:14px 17px;margin-bottom:20px;color:#376751;position:sticky;top:0;z-index:5;box-shadow:0 3px 12px #1f4b3610}
.loading-notice>div{flex:1;min-width:0}.loading-notice strong{display:block;font-weight:500;font-size:12px;line-height:1.7}.loading-notice small{display:block;color:#7a9481;font-size:10px;line-height:1.7;margin-top:3px}.loading-dots{display:flex;gap:4px}.loading-dots i{height:4px;width:4px;background:#71997c;border-radius:50%;animation:loading-pulse 1.2s ease-in-out infinite}.loading-dots i:nth-child(2){animation-delay:.15s}.loading-dots i:nth-child(3){animation-delay:.3s}@keyframes loading-pulse{0%,100%{opacity:.3}50%{opacity:1}}@media(prefers-reduced-motion:reduce){.spin,.loading-dots i{animation:none}}@media(max-width:520px){.loading-notice{padding:12px;gap:9px}.loading-dots{display:none}}
</style>
