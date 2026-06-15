<script lang="ts">
  // The XClaw octopus mark: a domed mantle with a scalloped tentacle skirt and
  // two eyes. Body is currentColor; eyes are punched with the paper tone so the
  // glyph reads on any surface.
  let { size = 24 }: { size?: number } = $props();
</script>

<svg width={size} height={size} viewBox="0 0 100 100" aria-hidden="true">
  <path
    fill="currentColor"
    d="M22 58 C22 22 78 22 78 58 q-7 13 -14 0 q-7 13 -14 0 q-7 13 -14 0 q-7 13 -14 0 Z"
  />
  <circle cx="41" cy="50" r="4" fill="var(--paper)" />
  <circle cx="59" cy="50" r="4" fill="var(--paper)" />
</svg>
