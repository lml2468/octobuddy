<script lang="ts">
  // The XClaw octopus mark: a rounded mantle with a four-tentacle skirt and two
  // eyes. Body is currentColor; eyes are punched with the paper tone (plus a tiny
  // currentColor pupil) so the glyph stays friendly and reads on any surface.
  let { size = 24 }: { size?: number } = $props();
</script>

<svg width={size} height={size} viewBox="0 0 100 100" aria-hidden="true">
  <path
    fill="currentColor"
    d="M50 14
       C29 14 18 29 18 49
       c0 8 3 15 8 19
       q7.5 13 15 0 q7.5 13 15 0 q7.5 13 15 0
       c5 -4 8 -11 8 -19
       C82 29 71 14 50 14 Z"
  />
  <circle cx="41" cy="48" r="4.2" fill="rgba(0,0,0,0.32)" />
  <circle cx="59" cy="48" r="4.2" fill="rgba(0,0,0,0.32)" />
</svg>
