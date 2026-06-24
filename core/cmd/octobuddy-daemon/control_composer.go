package main

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/gateway"
	"github.com/lml2468/octobuddy/core/router"
)

// composerAttachmentLimit caps how many attachments one Composer send can
// carry — defense in depth on top of the per-attachment size cap. Generous
// vs the IM-inbound image limit (gateway.MaxImagesPerSend = 6) since text
// files inline cheaply.
const composerAttachmentLimit = 10

// materializeComposerAttachments writes each attachment's bytes into the
// Console session's sandbox using the same gateway/media writers IM inbound
// uses, then returns the concatenated prompt fragment the agent will see.
// The fragment is identical in shape to what an IM peer sending the same
// files would produce, so the agent's Read-tool prompting is uniform.
//
// Errors here bubble up to the control-bus caller (the GUI) as a session.send
// rejection so the operator gets immediate feedback ("file too large", "no
// sandbox configured") instead of a turn that silently lost its attachments.
func materializeComposerAttachments(gw *gateway.Gateway, uid string, atts []control.SessionAttachment) (string, error) {
	if len(atts) > composerAttachmentLimit {
		return "", fmt.Errorf("attachment count %d exceeds limit %d", len(atts), composerAttachmentLimit)
	}
	cwd, err := gw.SessionCwd(router.ChannelDM, uid)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox: %w", err)
	}
	if cwd == "" {
		return "", fmt.Errorf("attachments require a sandbox; bot has no cwdBase configured")
	}

	fileBlocks, imageRels, err := collectComposerAttachmentFragments(cwd, atts)
	if err != nil {
		return "", err
	}
	return renderComposerAttachmentPrompt(fileBlocks, imageRels), nil
}

func collectComposerAttachmentFragments(cwd string, atts []control.SessionAttachment) ([]string, []string, error) {
	var fileBlocks []string
	var imageRels []string
	imageBudget := 0
	for i, att := range atts {
		if err := collectComposerAttachmentFragment(cwd, i, att, &imageBudget, &fileBlocks, &imageRels); err != nil {
			return nil, nil, err
		}
	}
	return fileBlocks, imageRels, nil
}

func collectComposerAttachmentFragment(cwd string, index int, att control.SessionAttachment, imageBudget *int, fileBlocks *[]string, imageRels *[]string) error {
	body, derr := base64.StdEncoding.DecodeString(att.Data)
	if derr != nil {
		return fmt.Errorf("attachment %d (%q): decode base64: %w", index, att.Name, derr)
	}
	switch att.Kind {
	case "image":
		return collectComposerImageAttachment(cwd, index, att, body, imageBudget, imageRels)
	case "file", "":
		return collectComposerFileAttachment(cwd, index, att, body, fileBlocks)
	default:
		return fmt.Errorf("attachment %d (%q): unknown kind %q", index, att.Name, att.Kind)
	}
}

func collectComposerImageAttachment(cwd string, index int, att control.SessionAttachment, body []byte, imageBudget *int, imageRels *[]string) error {
	if *imageBudget >= gateway.MaxImagesPerSend {
		return fmt.Errorf("attachment %d (%q): image budget exceeded (max %d per send)",
			index, att.Name, gateway.MaxImagesPerSend)
	}
	rel, werr := gateway.WriteSandboxImage(cwd, att.Mime, body)
	if werr != nil {
		return fmt.Errorf("attachment %d (%q): %w", index, att.Name, werr)
	}
	*imageRels = append(*imageRels, rel)
	*imageBudget = *imageBudget + 1
	return nil
}

func collectComposerFileAttachment(cwd string, index int, att control.SessionAttachment, body []byte, fileBlocks *[]string) error {
	// File path: inline small text-like files (mirrors IM inbound), write
	// everything else to the sandbox + render a path hint.
	if gateway.ShouldInlineAsText(att.Name) && len(body) <= gateway.MaxInlineFileBytes {
		*fileBlocks = append(*fileBlocks, gateway.RenderInlinedFileFragment(att.Name, body))
		return nil
	}
	rel, werr := gateway.WriteSandboxFile(cwd, att.Name, body)
	if werr != nil {
		return fmt.Errorf("attachment %d (%q): %w", index, att.Name, werr)
	}
	*fileBlocks = append(*fileBlocks, gateway.RenderFileFragment(att.Name, rel))
	return nil
}

func renderComposerAttachmentPrompt(fileBlocks, imageRels []string) string {
	var b strings.Builder
	for _, blk := range fileBlocks {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(blk)
	}
	if img := gateway.RenderImageFragment(imageRels); img != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(img)
	}
	return b.String()
}
