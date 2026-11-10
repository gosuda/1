<script lang="ts">
	let { onclick }: { onclick: () => void } = $props();
</script>

<button
	{onclick}
	class="group hidden w-80 cursor-pointer items-center gap-2 rounded-full border border-gray-200/70 bg-white/90 py-2 pl-3.5 pr-2.5 text-sm text-gray-500 shadow-sm transition-all hover:-translate-y-0.5 hover:border-primary/40 hover:text-primary sm:inline-flex dark:border-white/10 dark:bg-white/5 dark:text-gray-400 dark:hover:border-primary-light/40 dark:hover:text-primary-light"
	aria-label="Search documentation"
>
	<svg class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
		<path stroke-linecap="round" stroke-linejoin="round" d="m21 21-5.197-5.197m0 0A7.5 7.5 0 1 0 5.196 5.196a7.5 7.5 0 0 0 10.607 10.607Z" />
	</svg>
	<span class="hidden text-sm sm:inline">Search docs...</span>
	<span class="flex-1"></span>
	<kbd class="hidden rounded-md border border-gray-300/60 bg-gray-100/80 px-1.5 py-0.5 font-sans text-[11px] font-medium text-gray-400 transition-colors group-hover:border-primary/40 group-hover:text-primary sm:inline dark:border-white/10 dark:bg-white/5 dark:text-gray-500 dark:group-hover:border-primary-light/40 dark:group-hover:text-primary-light">
		⌘K
	</kbd>
</button>
