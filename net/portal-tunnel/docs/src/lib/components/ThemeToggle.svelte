<script lang="ts">
	import { toggleMode, mode } from 'mode-watcher';
</script>

<button
	onclick={toggleMode}
	class="inline-flex h-12 w-12 shrink-0 cursor-pointer items-center justify-center rounded-full border border-gray-200/70 bg-white/90 text-gray-700 shadow-sm transition-all hover:-translate-y-0.5 hover:border-primary/40 hover:text-primary dark:border-white/10 dark:bg-white/5 dark:text-gray-300 dark:hover:border-primary-light/40 dark:hover:text-primary-light"
	aria-label="Toggle theme"
>
	{#if mode.current === 'dark'}
		<svg class="h-[18px] w-[18px]" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
			<path stroke-linecap="round" stroke-linejoin="round" d="M21.752 15.002A9.72 9.72 0 0118 15.75c-5.385 0-9.75-4.365-9.75-9.75 0-1.33.266-2.597.748-3.752A9.753 9.753 0 003 11.25C3 16.635 7.365 21 12.75 21a9.753 9.753 0 009.002-5.998z" />
		</svg>
	{:else}
		<svg class="h-[18px] w-[18px]" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
			<path stroke-linecap="round" stroke-linejoin="round" d="M12 3v2.25m6.364.386l-1.591 1.591M21 12h-2.25m-.386 6.364l-1.591-1.591M12 18.75V21m-4.773-4.227l-1.591 1.591M5.25 12H3m4.227-4.773L5.636 5.636M15.75 12a3.75 3.75 0 11-7.5 0 3.75 3.75 0 017.5 0z" />
		</svg>
	{/if}
</button>
