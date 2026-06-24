// Media/file materialization — the Go port of cc-channel-octo's inbound media
// handling (src/media-inbound.ts downloadInboundImage + src/inbound.ts
// tryResolveFile + src/file-inline-wrap.ts buildInlinedFileBody).
//
// The IM connector attaches media/file URLs to router.InboundMessage.Attachments
// WITHOUT downloading (it has no session cwd). The gateway materializes them in
// runTurn AFTER the session cwd is resolved and BEFORE driver.Query:
//
// - Images (PNG/JPEG/GIF/WebP) are downloaded into <cwd>/.octobuddy-media/ so the
// agent's Read tool can open them natively, and a Read hint is appended to
// THIS turn's prompt body (not stored history).
// - Small text files (<20 KiB) are inlined as base64 inside a <file_content>
// wrapper — base64's alphabet can't forge the closing tag, defeating the
// "--- file end ---" prompt-injection break-out (file-inline-wrap.ts S2).
// - Larger / binary files are downloaded to <cwd>/.octobuddy-media/ and described
// with a path hint.
//
// SSRF: every download (and every redirect hop) is re-validated against
// config.AssertPublicURL. The bot Authorization header is scoped per hop via the
// gateway's MediaAuth hook — it only travels to the same host as apiUrl, so a
// redirect to an attacker host drops the credential.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/lml2468/octobuddy/core/router"
	"github.com/lml2468/octobuddy/core/safepath"
	"github.com/lml2468/octobuddy/core/safety"
)

// InboundMediaDir is the subdir (under the session cwd) where downloaded inbound
// images and oversized files land. Mirrors INBOUND_MEDIA_DIR in media-inbound.ts
// (renamed for this project). It shares the session cwd partition, so the 7-day
// sandbox janitor reclaims it — no separate cleanup needed.
const InboundMediaDir = ".octobuddy-media"

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
	doImage := imageMaterializationPlan(cwd, atts)
	results := make([]attachmentMaterializationResult, len(atts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, mediaConcurrency)
	for i, att := range atts {
		if !shouldMaterializeAttachment(att, doImage[i]) {
			continue
		}
		wg.Add(1)
		go func(i int, att router.Attachment) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			results[i] = g.materializeOneAttachment(ctx, cwd, att)
		}(i, att)
	}
	wg.Wait()
	return renderMaterializedAttachments(results)
}

type attachmentMaterializationResult struct {
	imageRel  string
	fileBlock string
}

func imageMaterializationPlan(cwd string, atts []router.Attachment) []bool {
	doImage := make([]bool, len(atts))
	imageBudget := 0
	for i, att := range atts {
		if att.Kind == router.AttachmentImage && cwd != "" && imageBudget < maxImagesPerMessage {
			doImage[i] = true
			imageBudget++
		}
	}
	return doImage
}

func shouldMaterializeAttachment(att router.Attachment, doImage bool) bool {
	switch att.Kind {
	case router.AttachmentImage:
		return doImage
	case router.AttachmentFile:
		return true
	default:
		return false
	}
}

func (g *Gateway) materializeOneAttachment(ctx context.Context, cwd string, att router.Attachment) attachmentMaterializationResult {
	switch att.Kind {
	case router.AttachmentImage:
		rel, err := g.downloadImage(ctx, cwd, att.URL)
		if err != nil {
			glog().Warn("inbound image skipped", "err", err)
			return attachmentMaterializationResult{}
		}
		return attachmentMaterializationResult{imageRel: rel}
	case router.AttachmentFile:
		return attachmentMaterializationResult{fileBlock: g.resolveFile(ctx, cwd, att)}
	default:
		return attachmentMaterializationResult{}
	}
}

func renderMaterializedAttachments(results []attachmentMaterializationResult) string {
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

	if len(imageRelPaths) > 0 {
		b.WriteString("\n")
		b.WriteString(RenderImageFragment(imageRelPaths))
	}
	return b.String()
}

// downloadImage fetches url into <cwd>/.octobuddy-media/<uuid>-<safeName> and returns
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

	// agent owns `cwd` (Bash + bypass) — bare MkdirAll
	// would follow an agent-planted `.octobuddy-media → ~/.ssh/` and the
	// subsequent writeCapped would land attacker-supplied IM bytes under
	//.ssh/. safepath's dirfd walk refuses the symlinked entry. cwd
	// itself is operator-trusted as the sandbox root.
	if err := safepath.SafeMkdirAll(cwd, InboundMediaDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir media dir: %w", err)
	}
	name := fmt.Sprintf("%s-image.%s", uuid.NewString(), ext)
	dir := filepath.Join(cwd, InboundMediaDir)
	localPath := filepath.Join(dir, name)
	if err := writeCapped(cwd, filepath.Join(InboundMediaDir, name), resp.Body, maxImageBytes); err != nil {
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
		return renderFileDescription(filename, att.Size)
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
	head, n, err := readInlineProbe(resp.Body)
	if isFileReadFailure(err) {
		return fmt.Sprintf("[文件: %s - 网络错误: %v]", filename, err)
	}
	if n <= inlineFileMaxBytes {
		return buildInlinedFileBody(filename, head)
	}
	if cwd == "" {
		return fmt.Sprintf("[文件: %s - 过大未内联]", filename)
	}
	return g.materializeLargeFile(cwd, filename, head, resp.Body)
}

func isFileReadFailure(err error) bool {
	return err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)
}

func renderFileDescription(filename string, size int64) string {
	if size > 0 {
		return fmt.Sprintf("[文件: %s (%s)]", filename, formatBytes(size))
	}
	return fmt.Sprintf("[文件: %s]", filename)
}

func readInlineProbe(r io.Reader) ([]byte, int, error) {
	head := make([]byte, inlineProbeBytes)
	n, err := io.ReadFull(r, head)
	return head[:n], n, err
}

func (g *Gateway) materializeLargeFile(cwd, filename string, head []byte, body io.Reader) string {
	if err := safepath.SafeMkdirAll(cwd, InboundMediaDir, 0o755); err != nil {
		return fmt.Sprintf("[文件: %s - 下载错误: %v]", filename, err)
	}
	dir := filepath.Join(cwd, InboundMediaDir)
	safeName := sanitizeFileBaseName(filename)
	leaf := fmt.Sprintf("%s-%s", uuid.NewString(), safeName)
	localPath := filepath.Join(dir, leaf)
	src := io.MultiReader(strings.NewReader(string(head)), body)
	if err := writeCapped(cwd, filepath.Join(InboundMediaDir, leaf), src, maxFileDownloadBytes); err != nil {
		_ = os.Remove(localPath)
		return fmt.Sprintf("[文件: %s - %v]", filename, err)
	}
	rel, relErr := filepath.Rel(cwd, localPath)
	if relErr != nil {
		rel = localPath
	}
	return RenderFileFragment(filename, rel)
}
