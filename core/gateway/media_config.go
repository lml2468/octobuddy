package gateway

import "time"

// Download / inline caps (media-inbound.ts + inbound.ts + file-inline-wrap.ts).
const (
	maxImageBytes        = 5 * 1024 * 1024 // MAX_IMAGE_BYTES
	maxFileDownloadBytes = 5 * 1024 * 1024 // MAX_FILE_DOWNLOAD_BYTES
	inlineFileMaxBytes   = 20 * 1024       // INLINE_FILE_MAX_BYTES
	// inlineProbeBytes reads one byte past the inline cap so an over-cap file is
	// detected without reading it unboundedly (see resolveFile). Keep it exactly
	// inlineFileMaxBytes+1 — the overflow test (n > inlineFileMaxBytes) depends on it.
	inlineProbeBytes    = inlineFileMaxBytes + 1
	maxImagesPerMessage = 6 // MAX_IMAGES_PER_MESSAGE
	downloadTimeout     = 15 * time.Second
	maxRedirects        = 10
	mediaConcurrency    = 4 // max simultaneous attachment downloads per message
)

// allowedImageTypes maps the content types Claude can natively Read to a file
// extension. A response whose type isn't here is rejected (ALLOWED_IMAGE_TYPES).
var allowedImageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/jpg":  "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// textFileExtensions are inlined (text-like content), mirroring
// TEXT_FILE_EXTENSIONS in inbound.ts.
var textFileExtensions = map[string]bool{
	"txt": true, "md": true, "csv": true, "json": true, "xml": true,
	"yaml": true, "yml": true, "log": true, "py": true, "js": true,
	"ts": true, "tsx": true, "jsx": true, "mjs": true, "cjs": true,
	"go": true, "java": true, "rs": true, "c": true, "h": true,
	"cpp": true, "hpp": true, "cs": true, "rb": true, "php": true,
	"html": true, "htm": true, "css": true, "scss": true, "sass": true,
	"less": true, "sh": true, "bash": true, "zsh": true, "fish": true,
	"ps1": true, "toml": true, "ini": true, "conf": true, "cfg": true,
	"env": true, "sql": true, "graphql": true, "gql": true, "proto": true,
}

// MediaAuth returns the Authorization header value to use when fetching url, or
// "" to send no credential. The IM connector supplies it so the bot token is
// scoped to the apiUrl host (a redirect to another host drops it). Keeping it a
// hook preserves the gateway's IM-agnosticism — it never embeds a token.
type MediaAuth func(url string) string
