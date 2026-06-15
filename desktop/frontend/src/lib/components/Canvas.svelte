<script lang="ts">
  // The living watercolor page: soft pigment washes with organic, displaced
  // edges that drift on a slow breath, over a procedural paper grain, with the
  // margins pooling slightly darker. Pure CSS + SVG filters — cheap (filters
  // rasterize; the drift only translates), and it adapts light/dark via the
  // --wash-* / --grain-* CSS variables.
</script>

<div class="canvas" aria-hidden="true">
  <!-- Filter defs: wandering edges for the washes; fractal noise for paper tooth. -->
  <svg class="defs" width="0" height="0">
    <defs>
      <filter id="wc-edge" x="-20%" y="-20%" width="140%" height="140%">
        <feTurbulence type="fractalNoise" baseFrequency="0.011" numOctaves="2" seed="7" result="n" />
        <feDisplacementMap in="SourceGraphic" in2="n" scale="70" xChannelSelector="R" yChannelSelector="G" />
      </filter>
      <filter id="paper-grain">
        <feTurbulence type="fractalNoise" baseFrequency="0.86" numOctaves="2" stitchTiles="stitch" result="t" />
        <feColorMatrix in="t" type="saturate" values="0" />
      </filter>
    </defs>
  </svg>

  <div class="washes">
    <span class="blob b1"></span>
    <span class="blob b2"></span>
    <span class="blob b3"></span>
    <span class="blob b4"></span>
  </div>

  <svg class="grain"><rect width="100%" height="100%" filter="url(#paper-grain)" /></svg>

  <div class="vignette"></div>
</div>

<style>
  .canvas { position: fixed; inset: 0; z-index: 0; background: var(--paper); overflow: hidden; pointer-events: none; }
  .defs { position: absolute; }

  .washes { position: absolute; inset: -10%; filter: url(#wc-edge) blur(38px); mix-blend-mode: var(--wash-blend); opacity: var(--wash-opacity); }
  .blob { position: absolute; border-radius: 50%; }
  .b1 { width: 46vw; height: 46vw; left: 4%;  top: 2%;   background: radial-gradient(circle, var(--wash-1), transparent 66%); animation: drift1 42s ease-in-out infinite alternate; }
  .b2 { width: 40vw; height: 40vw; right: 2%; top: 8%;   background: radial-gradient(circle, var(--wash-2), transparent 66%); animation: drift2 51s ease-in-out infinite alternate; }
  .b3 { width: 44vw; height: 44vw; left: 8%;  bottom: 0%; background: radial-gradient(circle, var(--wash-3), transparent 66%); animation: drift3 47s ease-in-out infinite alternate; }
  .b4 { width: 38vw; height: 38vw; right: 6%; bottom: 4%; background: radial-gradient(circle, var(--wash-4), transparent 66%); animation: drift4 56s ease-in-out infinite alternate; }

  @keyframes drift1 { from { transform: translate(0, 0) scale(1); } to { transform: translate(5%, 4%) scale(1.08); } }
  @keyframes drift2 { from { transform: translate(0, 0) scale(1.05); } to { transform: translate(-4%, 5%) scale(1); } }
  @keyframes drift3 { from { transform: translate(0, 0) scale(1); } to { transform: translate(4%, -4%) scale(1.1); } }
  @keyframes drift4 { from { transform: translate(0, 0) scale(1.06); } to { transform: translate(-5%, -4%) scale(1); } }

  .grain { position: absolute; inset: 0; width: 100%; height: 100%; mix-blend-mode: var(--grain-blend); opacity: var(--grain-opacity); }

  .vignette { position: absolute; inset: 0; background: radial-gradient(120% 120% at 50% 40%, transparent 55%, color-mix(in srgb, var(--ink) 12%, transparent)); mix-blend-mode: multiply; }

  @media (prefers-reduced-motion: reduce) {
    .blob { animation: none; }
  }
</style>
