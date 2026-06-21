// Media/file materialization — the Go port of cc-channel-octo's inbound media
// handling (src/media-inbound.ts downloadInboundImage + src/inbound.ts
// tryResolveFile + src/file-inline-wrap.ts buildInlinedFileBody).
//
// The IM connector attaches media/file URLs to router.InboundMessage.Attachments
// WITHOUT downloading (it has no session cwd). The gateway materializes them in
// runTurn AFTER the session cwd is resolved and BEFORE driver.Query:
//
//   - Images (PNG/JPEG/GIF/WebP) are downloaded into <cwd>/.xclaw-media/ so the
//     agent's Read tool can open them natively, and a Read hint is appended to
//     THIS turn's prompt body (not stored history).
//   - Small text files (<20 KiB) are inlined as base64 inside a <file_content>
//     wrapper — base64's alphabet can't forge the closing tag, defeating the
//     "--- file end ---" prompt-injection break-out (file-inline-wrap.ts S2).
//   - Larger / binary files are downloaded to <cwd>/.xclaw-media/ and described
//     with a path hint.
//
// SSRF: every download (and every redirect hop) is re-validated against
// config.AssertPublicURL. The bot Authorization header is scoped per hop via the
// gateway's MediaAuth hook — it only travels to the same host as apiUrl, so a
// redirect to an attacker host drops the credential.
package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/lml2468/xclaw/core/config"
	"github.com/lml2468/xclaw/core/router"
	"github.com/lml2468/xclaw/core/safety"
)

// InboundMediaDir is the subdir (under the session cwd) where downloaded inbound
// images and oversized files land. Mirrors INBOUND_MEDIA_DIR in media-inbound.ts
// (renamed for this project). It shares the session cwd partition, so the 7-day
// sandbox janitor reclaims it — no separate cleanup needed.
const InboundMediaDir = ".xclaw-media"

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

// mediaHTTPClient is the media downloader's transport.
//
//   - redirect: manual — we walk the chain ourselves so each hop is
//     SSRF-revalidated and the Authorization header is recomputed per hop
//     (fetchWithRedirectGuard parity).
//   - DialControl: the actual socket address chosen by the resolver is
//     re-checked against the private/local ranges at *dial time*. This closes the
//     DNS-rebinding TOCTOU: AssertPublicURL resolves once for the policy check,
//     but the transport resolves again to dial — a hostile authoritative DNS
//     could return a public IP to the first lookup and 169.254.169.254 / a
//     private IP to the second. Validating in Control (which runs on the exact
//     address being connected) makes the check authoritative for the connection.
//   - explicit Transport timeouts + per-host conn cap so a slow/hostile endpoint
//     can't tie up connections (the ctx deadline still bounds the whole fetch).
var mediaHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   downloadTimeout,
			KeepAlive: 30 * time.Second,
			Control:   dialControlGuard,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxConnsPerHost:       8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: downloadTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// dialControlGuard rejects a connection whose resolved destination address is in
// a private/loopback/link-local/CGN/unspecified range. Runs after DNS resolution
// on the concrete address the kernel is about to connect to, so it defeats DNS
// rebinding that AssertPublicURL's earlier lookup cannot (the resolver may return
// a different IP at dial time).
func dialControlGuard(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("media dial: bad address %q: %w", address, err)
	}
	if config.IsPrivateOrLocalAddress(host) {
		return fmt.Errorf("media dial: refusing private/local address %s", host)
	}
	return nil
}

// materializeAttachments downloads/inlines the message's attachments into cwd and
// returns the prompt addition for THIS turn only: a trailing Read hint for
// downloaded images and any inline <file_content> blocks for small text files.
// It never returns an error — a failed attachment degrades to nothing (the
// original URL marker the connector put in the text remains the fallback).
//
// When cwd is "" (sandbox disabled) images can't be materialized for the Read
// tool; image attachments are skipped (the text already carries the URL marker).
// Text-file inlining still works since it doesn't need the cwd.
//
// Downloads run concurrently (bounded by mediaConcurrency) so N attachments cost
// roughly one download's wall time instead of the sum — a slow URL can't serialize
// the whole turn for downloadTimeout × N before driver.Query. Output order is
// preserved (results are assembled by attachment index after the fan-out joins).
func (g *Gateway) materializeAttachments(ctx context.Context, cwd string, atts []router.Attachment) string {
	if len(atts) == 0 {
		return ""
	}

	// Honor maxImagesPerMessage deterministically by attachment order, decided
	// up front so concurrency doesn't make the cutoff nondeterministic.
	doImage := make([]bool, len(atts))
	imageBudget := 0
	for i, att := range atts {
		if att.Kind == router.AttachmentImage && cwd != "" && imageBudget < maxImagesPerMessage {
			doImage[i] = true
			imageBudget++
		}
	}

	// Per-attachment result, filled concurrently and assembled in order. Each
	// goroutine writes only its own index, so no shared-write synchronization is
	// needed beyond the WaitGroup join.
	type result struct {
		imageRel  string // non-empty → a downloaded image
		fileBlock string // non-empty → an inline/description block
	}
	results := make([]result, len(atts))

	var wg sync.WaitGroup
	sem := make(chan struct{}, mediaConcurrency)
	for i, att := range atts {
		switch att.Kind {
		case router.AttachmentImage:
			if !doImage[i] {
				continue
			}
		case router.AttachmentFile:
			// always handled
		default:
			continue
		}
		wg.Add(1)
		go func(i int, att router.Attachment) {
			defer wg.Done()
			// Bound by mediaConcurrency, but honor ctx so a per-turn cancel
			// (timeout / shutdown) releases the goroutine immediately
			// instead of waiting on a busy slot whose holder may be slow
			// to observe cancellation itself (round 12 G6).
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			switch att.Kind {
			case router.AttachmentImage:
				rel, err := g.downloadImage(ctx, cwd, att.URL)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[gateway] inbound image skipped: %v\n", err)
					return
				}
				results[i].imageRel = rel
			case router.AttachmentFile:
				results[i].fileBlock = g.resolveFile(ctx, cwd, att)
			}
		}(i, att)
	}
	wg.Wait()

	// Assemble in attachment order: file blocks inline; image paths gather into a
	// single trailing Read hint.
	var b strings.Builder
	var imageRelPaths []string
	for i := range results {
		if results[i].fileBlock != "" {
			b.WriteString("\n")
			b.WriteString(results[i].fileBlock)
		} else if results[i].imageRel != "" {
			imageRelPaths = append(imageRelPaths, results[i].imageRel)
		}
	}

	if len(imageRelPaths) == 1 {
		fmt.Fprintf(&b, "\n[已下载图片到本地: %s — 请用 Read 工具查看]", imageRelPaths[0])
	} else if len(imageRelPaths) > 1 {
		fmt.Fprintf(&b, "\n[已下载 %d 张图片到本地: %s — 请用 Read 工具逐个查看]",
			len(imageRelPaths), strings.Join(imageRelPaths, ", "))
	}
	return b.String()
}

// downloadImage fetches url into <cwd>/.xclaw-media/<uuid>-<safeName> and returns
// the path relative to cwd (what the Read hint shows). Mirrors
// downloadInboundImage: SSRF-checked, per-hop token scoping, content-type gate,
// 5 MB cap, downloadTimeout deadline.
func (g *Gateway) downloadImage(ctx context.Context, cwd, rawURL string) (string, error) {
	resp, err := g.fetchGuarded(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed HTTP %d", resp.StatusCode)
	}
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	rawType := strings.ToLower(strings.TrimSpace(ct))
	ext, ok := allowedImageTypes[rawType]
	if !ok {
		if rawType == "" {
			rawType = "未知"
		}
		return "", fmt.Errorf("unsupported image type: %s", rawType)
	}

	dir := filepath.Join(cwd, InboundMediaDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir media dir: %w", err)
	}
	name := fmt.Sprintf("%s-image.%s", uuid.NewString(), ext)
	localPath := filepath.Join(dir, name)
	if err := writeCapped(localPath, resp.Body, maxImageBytes); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(cwd, localPath)
	if err != nil {
		rel = localPath
	}
	return rel, nil
}

// resolveFile mirrors tryResolveFile + buildInlinedFileBody. It returns the
// prompt block for the file (inline <file_content>, a downloaded-path hint, or a
// short description) — never an error; failures degrade to a description so the
// turn still proceeds.
func (g *Gateway) resolveFile(ctx context.Context, cwd string, att router.Attachment) string {
	// att.Name is already sanitized by the connector; re-sanitize defensively for
	// the label/attribute so a downstream change can't reintroduce injection.
	filename := safety.SanitizeDisplayName(att.Name, "未知文件")
	ext := extractExtension(att.URL, filename)

	if !textFileExtensions[ext] {
		if att.Size > 0 {
			return fmt.Sprintf("[文件: %s (%s)]", filename, formatBytes(att.Size))
		}
		return fmt.Sprintf("[文件: %s]", filename)
	}
	if att.Size > 0 && att.Size > maxFileDownloadBytes {
		return fmt.Sprintf("[文件: %s (%s) - 超过下载上限 %s]",
			filename, formatBytes(att.Size), formatBytes(maxFileDownloadBytes))
	}

	resp, err := g.fetchGuarded(ctx, att.URL)
	if err != nil {
		return fmt.Sprintf("[文件: %s - 拒绝下载: %v]", filename, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("[文件: %s - 下载失败 HTTP %d]", filename, resp.StatusCode)
	}

	// Read up to the inline cap + 1 byte to detect overflow.
	head := make([]byte, inlineProbeBytes)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Sprintf("[文件: %s - 网络错误: %v]", filename, err)
	}
	head = head[:n]

	if n <= inlineFileMaxBytes {
		// Inline: base64-wrap so the content can't break out of the tag.
		return buildInlinedFileBody(filename, head)
	}

	// Exceeds inline cap — download to disk (capped at MAX_FILE_DOWNLOAD_BYTES).
	if cwd == "" {
		return fmt.Sprintf("[文件: %s - 过大未内联]", filename)
	}
	dir := filepath.Join(cwd, InboundMediaDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Sprintf("[文件: %s - 下载错误: %v]", filename, err)
	}
	safeName := sanitizeFileBaseName(filename)
	localPath := filepath.Join(dir, fmt.Sprintf("%s-%s", uuid.NewString(), safeName))
	// Concatenate the already-read head with the remaining body, capped.
	body := io.MultiReader(strings.NewReader(string(head)), resp.Body)
	if err := writeCapped(localPath, body, maxFileDownloadBytes); err != nil {
		_ = os.Remove(localPath)
		return fmt.Sprintf("[文件: %s - %v]", filename, err)
	}
	rel, relErr := filepath.Rel(cwd, localPath)
	if relErr != nil {
		rel = localPath
	}
	return fmt.Sprintf("[文件: %s]\n本地路径: %s", filename, rel)
}

// fetchGuarded performs an SSRF-guarded GET, manually walking redirects so each
// hop is re-validated and the Authorization header is recomputed per hop
// (fetchWithRedirectGuard + assertPublicUrl parity).
func (g *Gateway) fetchGuarded(ctx context.Context, rawURL string) (*http.Response, error) {
	assertPublic := g.assertPublic
	if assertPublic == nil {
		assertPublic = config.AssertPublicURL
	}
	client := g.mediaClient
	if client == nil {
		client = mediaHTTPClient
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	current := rawURL
	for hop := 0; hop <= maxRedirects; hop++ {
		if err := assertPublic(ctx, current); err != nil {
			cancel()
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		if g.mediaAuth != nil {
			if h := g.mediaAuth(current); h != "" {
				req.Header.Set("Authorization", h)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if loc == "" {
				cancel()
				return nil, fmt.Errorf("redirect without Location")
			}
			base, perr := url.Parse(current)
			if perr != nil {
				cancel()
				return nil, perr
			}
			ref, perr := url.Parse(loc)
			if perr != nil {
				cancel()
				return nil, perr
			}
			current = base.ResolveReference(ref).String()
			continue
		}
		// Terminal response — wrap the body so closing it also cancels the ctx.
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	cancel()
	return nil, fmt.Errorf("too many redirects (started at %s)", rawURL)
}

// cancelOnCloseBody cancels the download context when the body is closed, so the
// per-download timeout's resources are always released.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// writeCapped streams src into path, deleting the partial file and returning an
// error if more than max bytes arrive.
func writeCapped(path string, src io.Reader, max int64) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	// limit = max+1 so we can detect overflow without reading unboundedly.
	n, err := io.Copy(f, io.LimitReader(src, max+1))
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write file: %w", err)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close file: %w", closeErr)
	}
	if n > max {
		_ = os.Remove(path)
		return fmt.Errorf("exceeds size cap %s", formatBytes(max))
	}
	if n == 0 {
		_ = os.Remove(path)
		return fmt.Errorf("empty response")
	}
	return nil
}

// buildInlinedFileBody frames file content as a base64 <file_content> block.
// The base64 alphabet ([A-Za-z0-9+/=]) contains none of <, /, > so the content
// cannot forge the closing tag (file-inline-wrap.ts S2 defense).
func buildInlinedFileBody(filename string, content []byte) string {
	safeName := sanitizeFilenameForAttribute(filename)
	b64 := base64.StdEncoding.EncodeToString(content)
	return fmt.Sprintf("[文件: %s]\n<file_content name=%q encoding=\"base64\" bytes=\"%d\">\n%s\n</file_content>",
		filename, safeName, len(content), b64)
}

// sanitizeFilenameForAttribute strips characters that could break out of the
// name="..." attribute or be misread as the closing tag (file-inline-wrap.ts).
func sanitizeFilenameForAttribute(name string) string {
	r := strings.NewReplacer(
		"<", "_", ">", "_", "\"", "_", "'", "_", "\\", "_",
		"\r", "_", "\n", "_", "\t", "_",
	)
	out := r.Replace(name)
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

// sanitizeFileBaseName reduces a filename to [A-Za-z0-9._-] for an on-disk name
// (tryResolveFile temp-path naming), falling back to "file".
func sanitizeFileBaseName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "file"
	}
	s := b.String()
	// A name that is only dots (".", "..", …) is harmless here because callers
	// always prefix a uuid, but collapse it anyway so an on-disk name never reads
	// like a path traversal segment.
	if strings.Trim(s, ".") == "" {
		return "file"
	}
	return s
}

// extractExtension returns the lowercase extension from the URL path, falling
// back to the filename. Matches extractExtension in inbound.ts (<=8 chars).
func extractExtension(rawURL, fallbackName string) string {
	if u, err := url.Parse(rawURL); err == nil {
		if ext := extFromPath(u.Path); ext != "" {
			return ext
		}
	}
	return extFromPath(fallbackName)
}

func extFromPath(p string) string {
	// Drop any query/fragment that rode along on a fallback name (the URL-parsed
	// path is already clean, but a raw filename may carry "?v=2" etc.).
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	i := strings.LastIndex(p, ".")
	if i < 0 || i == len(p)-1 {
		return ""
	}
	ext := strings.ToLower(p[i+1:])
	// stop at the first path-ish separator that slipped in
	if strings.ContainsAny(ext, "/\\") || len(ext) > 8 {
		return ""
	}
	return ext
}

// formatBytes renders a byte count for a human-readable label (formatBytes in
// inbound.ts).
func formatBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * 1024
		gb = 1024 * 1024 * 1024
	)
	switch {
	case n < kb:
		return fmt.Sprintf("%dB", n)
	case n < mb:
		return fmt.Sprintf("%.1fKB", float64(n)/kb)
	case n < gb:
		return fmt.Sprintf("%.1fMB", float64(n)/mb)
	default:
		return fmt.Sprintf("%.1fGB", float64(n)/gb)
	}
}
