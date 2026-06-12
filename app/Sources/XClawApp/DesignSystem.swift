import SwiftUI
import AppKit

// MARK: - Brand

extension Color {
    /// XClaw brand accent — adaptive light/dark, defined in code (not an asset
    /// catalog) so it resolves identically in `swift build` dev runs and packaged
    /// builds. Sampled from the app icon's ocean-blue; the dark variant is
    /// brighter for contrast on dark surfaces.
    static let brand = Color(nsColor: NSColor(name: "XClawBrand") { appearance in
        let isDark = appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
        return isDark
            ? NSColor(srgbRed: 0.40, green: 0.66, blue: 1.00, alpha: 1)
            : NSColor(srgbRed: 0.16, green: 0.46, blue: 0.92, alpha: 1)
    })
}

// MARK: - Liquid Glass + depth

extension View {
    /// Liquid Glass on macOS 26 (Tahoe), with a system-material fallback on
    /// macOS 14–15. Use on *floating* surfaces (banners, the compose field, the
    /// menu card) — not full-bleed bars, where `.bar` remains correct.
    @ViewBuilder
    func liquidGlass(in shape: some Shape, fallback material: Material = .regularMaterial) -> some View {
        if #available(macOS 26.0, *) {
            self.glassEffect(.regular, in: shape)
        } else {
            self.background(material, in: shape)
        }
    }

    /// Subtle, layered floating-card depth per the Liquid Glass depth guidance —
    /// two soft shadows rather than one heavy drop.
    func floatingShadow() -> some View {
        self
            .shadow(color: .black.opacity(0.07), radius: 8, y: 3)
            .shadow(color: .black.opacity(0.04), radius: 2, y: 1)
    }
}

// MARK: - Theme tokens

enum AppTheme {
    /// The global motion curve — hovers, selections, insertions, focus.
    static let spring = Animation.spring(response: 0.4, dampingFraction: 0.8)
}

// MARK: - Typography

/// App type ramp: rounded, slightly-tracked titles per the design spec. Apply
/// via `.appFont(.title)`. Use `.monospacedDigit()` at the call site for counts.
enum AppFont {
    case largeTitle, title, headline, body, callout, caption

    var font: Font {
        switch self {
        case .largeTitle: return .system(size: 26, weight: .bold, design: .rounded)
        case .title:      return .system(size: 20, weight: .semibold, design: .rounded)
        case .headline:   return .system(size: 15, weight: .semibold, design: .rounded)
        case .body:       return .system(size: 13, weight: .regular)
        case .callout:    return .system(size: 12, weight: .regular)
        case .caption:    return .system(size: 11, weight: .regular)
        }
    }
    var tracking: CGFloat {
        switch self {
        case .largeTitle, .title, .headline: return -0.2
        default: return 0
        }
    }
}

extension View {
    func appFont(_ f: AppFont) -> some View { font(f.font).tracking(f.tracking) }
}

// MARK: - Interaction modifiers

extension View {
    /// Focus ring: brand 2pt stroke + soft glow, spring-animated. For text inputs.
    func focusRing(_ focused: Bool, cornerRadius: CGFloat = 9) -> some View {
        overlay(
            RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                .strokeBorder(Color.brand, lineWidth: focused ? 2 : 0)
        )
        .shadow(color: Color.brand.opacity(focused ? 0.30 : 0), radius: 4)
        .animation(AppTheme.spring, value: focused)
    }

    /// Subtle hover lift (scale + spring); tracks its own hover state.
    func hoverScale(_ scale: CGFloat = 1.02) -> some View { modifier(HoverScale(scale: scale)) }

    /// Selected-row highlight pill that slides between rows via matchedGeometry.
    @ViewBuilder
    func selectionPill(_ selected: Bool, in namespace: Namespace.ID, cornerRadius: CGFloat = 8) -> some View {
        background {
            if selected {
                RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                    .fill(Color.brand.opacity(0.14))
                    .matchedGeometryEffect(id: "selectionPill", in: namespace)
            }
        }
    }

    /// A small material capsule tag (11pt text in ultraThinMaterial).
    func capsuleTag() -> some View {
        appFont(.caption)
            .padding(.horizontal, 8).padding(.vertical, 3)
            .background(.ultraThinMaterial, in: Capsule())
            .overlay(Capsule().strokeBorder(.white.opacity(0.12), lineWidth: 0.5))
    }

    /// Loading shimmer for skeleton placeholders.
    func shimmering() -> some View { modifier(Shimmer()) }
}

private struct HoverScale: ViewModifier {
    @State private var hovering = false
    var scale: CGFloat = 1.02
    func body(content: Content) -> some View {
        content
            .scaleEffect(hovering ? scale : 1)
            .animation(AppTheme.spring, value: hovering)
            .onHover { hovering = $0 }
    }
}

private struct Shimmer: ViewModifier {
    @State private var phase: CGFloat = -1
    func body(content: Content) -> some View {
        content.overlay(
            GeometryReader { geo in
                LinearGradient(colors: [.clear, .white.opacity(0.35), .clear],
                               startPoint: .leading, endPoint: .trailing)
                    .frame(width: geo.size.width * 0.6)
                    .offset(x: geo.size.width * phase)
                    .blendMode(.plusLighter)
                    .allowsHitTesting(false)
            }
        )
        .onAppear {
            withAnimation(.linear(duration: 1.2).repeatForever(autoreverses: false)) { phase = 1.6 }
        }
    }
}

// MARK: - Markdown message rendering

/// A coarse block split of an agent message: fenced ```code``` blocks vs prose.
/// Inline markup (bold/italic/`code`/links) inside prose is rendered by
/// `AttributedString`; this only needs to peel out fenced code so it can be
/// shown in a monospaced, copyable panel.
enum MarkdownBlock: Equatable {
    case prose(String)
    case code(String, language: String?)

    static func parse(_ text: String) -> [MarkdownBlock] {
        var blocks: [MarkdownBlock] = []
        var prose: [String] = []
        func flushProse() {
            let joined = prose.joined(separator: "\n")
                .trimmingCharacters(in: .whitespacesAndNewlines)
            if !joined.isEmpty { blocks.append(.prose(joined)) }
            prose.removeAll()
        }
        let lines = text.components(separatedBy: "\n")
        var i = 0
        while i < lines.count {
            let trimmed = lines[i].trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("```") {
                flushProse()
                let lang = String(trimmed.dropFirst(3)).trimmingCharacters(in: .whitespaces)
                var code: [String] = []
                i += 1
                while i < lines.count, !lines[i].trimmingCharacters(in: .whitespaces).hasPrefix("```") {
                    code.append(lines[i]); i += 1
                }
                blocks.append(.code(code.joined(separator: "\n"), language: lang.isEmpty ? nil : lang))
            } else {
                prose.append(lines[i])
            }
            i += 1
        }
        flushProse()
        return blocks
    }
}

/// Renders a message body as Markdown: prose with inline styling (line breaks
/// preserved), fenced code as styled monospaced panels with a copy affordance.
struct MarkdownMessage: View {
    let text: String

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(MarkdownBlock.parse(text).enumerated()), id: \.offset) { _, block in
                switch block {
                case .prose(let s):
                    Text(Self.attributed(s))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                case .code(let code, let lang):
                    CodeBlock(code: code, language: lang)
                }
            }
        }
    }

    /// Parse inline markup while preserving author line breaks (so multi-line
    /// agent output keeps its shape). Falls back to plain text on a parse error.
    private static func attributed(_ s: String) -> AttributedString {
        let opts = AttributedString.MarkdownParsingOptions(
            interpretedSyntax: .inlineOnlyPreservingWhitespace,
            failurePolicy: .returnPartiallyParsedIfPossible)
        return (try? AttributedString(markdown: s, options: opts)) ?? AttributedString(s)
    }
}

/// A monospaced, horizontally-scrollable code panel with an optional language
/// tag and a hover-revealed copy button.
private struct CodeBlock: View {
    let code: String
    let language: String?
    @State private var hovering = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            if let language {
                Text(language)
                    .font(.caption2.weight(.medium))
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 10)
                    .padding(.top, 6)
            }
            ScrollView(.horizontal, showsIndicators: false) {
                Text(code)
                    .font(.system(.callout, design: .monospaced))
                    .textSelection(.enabled)
                    .padding(10)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color(nsColor: .textBackgroundColor).opacity(0.55),
                    in: RoundedRectangle(cornerRadius: 10, style: .continuous))
        .overlay(
            RoundedRectangle(cornerRadius: 10, style: .continuous)
                .stroke(.quaternary, lineWidth: 1)
        )
        .overlay(alignment: .topTrailing) {
            Button {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(code, forType: .string)
            } label: {
                Image(systemName: "doc.on.doc")
            }
            .buttonStyle(.borderless)
            .padding(6)
            .opacity(hovering ? 1 : 0)
            .accessibilityLabel("Copy code")
        }
        .onHover { hovering = $0 }
    }
}

// MARK: - Octopus brand glyph

/// The XClaw octopus mark as a vector `Shape` (mantle + tentacle skirt, eyes
/// punched out via even-odd fill). Used for in-app glyphs (e.g. the menu-bar
/// popover header). Monochrome → tints cleanly.
struct OctopusShape: Shape {
    func path(in rect: CGRect) -> Path {
        var p = Path()
        let w = rect.width, h = rect.height
        let x = rect.minX, y = rect.minY
        // mantle / head
        p.addEllipse(in: CGRect(x: x + w*0.20, y: y + h*0.06, width: w*0.60, height: h*0.58))
        // tentacle skirt: four blobs across the bottom, inner pair longer
        let baseY = y + h*0.50
        let r = w*0.12
        for (i, fx) in [CGFloat(0.27), 0.43, 0.57, 0.73].enumerated() {
            let drop = (i == 1 || i == 2) ? h*0.34 : h*0.26
            p.addEllipse(in: CGRect(x: x + w*fx - r, y: baseY, width: 2*r, height: drop))
        }
        // eyes (punched out under even-odd fill)
        p.addEllipse(in: CGRect(x: x + w*0.38, y: y + h*0.26, width: w*0.09, height: h*0.11))
        p.addEllipse(in: CGRect(x: x + w*0.53, y: y + h*0.26, width: w*0.09, height: h*0.11))
        return p
    }
}
