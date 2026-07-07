// Package prompt owns the PURE assembly of a turn's prompts — the part of the
// pipeline that carries the prompt-injection defenses and nothing else. The
// gateway gathers the raw, already-resolved inputs (running the resolvers,
// reading GROUP.md, deciding owner-trust, mutating group-context cursors); this
// package only WRAPS and ORDERS them, so the injection-safety invariants live in
// one dependency-free, independently-testable place.
//
// Invariants this package enforces (mirrors the prior inline gateway logic,
// byte-identical via agent.SystemPrompt.Flatten):
//   - SecurityPrefix is always the Mandatory segment and appears first — it can
//     never be displaced by operator or untrusted content.
//   - Operator-trusted segments (SOUL/AGENTS, roster, persona, bootstrap) are
//     TrustedText, kept in a fixed order.
//   - Untrusted content (the GROUP.md handbook body, the group-message delta,
//     the current-message body) is escaped via safety.SafeBody /
//     SanitizePromptBody and fenced under privileged markers, so a crafted body
//     cannot forge prompt structure.
//
// It imports only core/agent (the neutral SystemPrompt type) and core/safety
// (the escaping choke-point) — never router/groupctx/persona/store — so it stays
// a leaf.
package prompt

import (
	"strings"

	"github.com/lml2468/octobuddy/core/agent"
	"github.com/lml2468/octobuddy/core/safety"
)

// bootstrapHeader labels the first-run ritual block in the assembled system
// prompt. Kept in sync with config.BootstrapName by a compile-time assertion in
// the gateway test package (TestBootstrapHeaderMatchesName) — this package does
// not import config (it stays dependency-free), so the header is a local literal
// rather than a derived string. Exported so the gateway's cross-check can read it.
const BootstrapHeader = "## BOOTSTRAP.md (first-run ritual — owner only)"

// SystemInputs are the already-gathered, per-turn raw strings the gateway hands
// to AssembleSystem. The gateway resolves each one (per-turn resolvers, roster
// snapshot, GROUP.md read, owner-trust gate, persona) — this package does the
// SafeText wrapping + ordering only. Every field is optional ("" → the segment
// is elided), matching the prior inline behavior.
type SystemInputs struct {
	// OperatorPrompt is the operator-trusted SOUL.md + AGENTS.md block
	// (effectiveSystemPrompt). "" → only the SecurityPrefix leads.
	OperatorPrompt string
	// RosterPrefix is the synthesized group member list. "" for DMs and for
	// groups with no learned members.
	RosterPrefix string
	// Handbook is the RAW (unescaped) GROUP.md body the gateway read via
	// safepath.SafeRead. Escaped + fenced under GroupHandbookHeader here. Injected
	// only when IsGroup and non-empty.
	Handbook string
	// IsGroup gates handbook injection (the handbook is a group-only concern).
	IsGroup bool
	// PersonaGroup / PersonaHint are the OBO persona-clone segments
	// (persona.BuildGroupSystemPrompt / ComposeHint). "" when not a persona clone.
	PersonaGroup string
	PersonaHint  string
	// Bootstrap is the first-run ritual body — set by the gateway ONLY when the
	// turn is owner-trusted and BOOTSTRAP.md still exists, else "". Prefixed with
	// BootstrapHeader here.
	Bootstrap string
}

// AssembleSystem returns the structured agent.SystemPrompt: SecurityPrefix as
// the non-overridable Mandatory segment, then every non-empty trusted/escaped
// segment as Persona in the established order:
//
//	SecurityPrefix → OperatorPrompt → RosterPrefix → Handbook → PersonaGroup →
//	PersonaHint → Bootstrap
//
// The Background segment stays empty (migration phase 2): the handbook is kept
// inline in Persona so Flatten() is byte-identical to the previous flat
// assembly. Moving it to Background (after all trusted segments) is a deliberate,
// separately-reviewed change deferred to a later phase.
func AssembleSystem(in SystemInputs) agent.SystemPrompt {
	// parts[0] is always the SecurityPrefix (the Mandatory segment); everything
	// after it is operator-trusted / escaped Persona.
	parts := []safety.SafeText{safety.TrustedText(safety.SecurityPrefix)}
	if in.OperatorPrompt != "" {
		parts = append(parts, safety.TrustedText(in.OperatorPrompt))
	}
	if in.RosterPrefix != "" {
		parts = append(parts, safety.TrustedText(in.RosterPrefix))
	}
	if block := handbookBlock(in); block != "" {
		parts = append(parts, safety.TrustedText(block))
	}
	if in.PersonaGroup != "" {
		parts = append(parts, safety.TrustedText(in.PersonaGroup))
	}
	if in.PersonaHint != "" {
		parts = append(parts, safety.TrustedText(in.PersonaHint))
	}
	if in.Bootstrap != "" {
		parts = append(parts, safety.TrustedText(BootstrapHeader+"\n\n"+in.Bootstrap))
	}

	sp := agent.SystemPrompt{Mandatory: parts[0].String()}
	for _, p := range parts[1:] {
		sp.Persona = append(sp.Persona, p.String())
	}
	return sp
}

// handbookBlock escapes the untrusted GROUP.md body and fences it under the
// privileged GroupHandbookHeader — a crafted handbook can neither forge a second
// marker/role label nor displace the operator-trusted segments above it. Returns
// "" for DMs, when the body is absent, or when it trims to empty. The header is a
// trusted literal (our privileged marker); the body is escaped via
// SanitizePromptBody. The combined block is minted as TrustedText by the caller
// because it is already escaped (TrustedText documents that the HEADER is ours).
func handbookBlock(in SystemInputs) string {
	if !in.IsGroup {
		return ""
	}
	body := strings.TrimSpace(in.Handbook)
	if body == "" {
		return ""
	}
	return safety.GroupHandbookHeader + "\n" + safety.SanitizePromptBody(body)
}

// RenderGroup builds the USER prompt for a group turn: the escaped
// [Recent group messages] delta block, then the current-message anchor, then the
// escaped current-message body. Pure (no side effects) — the gateway owns the
// cursor/backfill mutations and calls this only to render the final string. The
// DM path is a raw passthrough handled by the gateway, not this function.
//
// CRITICAL: the current-message body is untrusted and escaped via safety.SafeBody
// so a crafted body cannot forge a second [Current message …] anchor or a fake
// [Recent group messages] header below the real anchor. The delta block is
// escaped once here via SanitizePromptBody (header + raw bodies together).
func RenderGroup(deltaText, currentText string) string {
	var b strings.Builder
	if deltaText != "" {
		b.WriteString(safety.SanitizePromptBody(deltaText))
		b.WriteString("\n")
	}
	b.WriteString(safety.CurrentMessageAnchor)
	b.WriteString("\n")
	b.WriteString(safety.SafeBody(currentText).String())
	return b.String()
}
