import Testing
import Foundation
@testable import XClawCore

@Test
func protocolVersionMatchesContract() {
    #expect(XClawCore.protocolVersion == 1)
}

@Test
func envelopeCodecRoundTrip() throws {
    let env = try ControlCodec.command(id: "1", type: "session.send",
                                       body: SessionSendBody(uid: "u1", text: "hi"))
    let line = try ControlCodec.encode(env)
    #expect(line.last == 0x0A)
    let decoded = try ControlCodec.decode(line.dropLast()) // strip newline
    #expect(decoded.kind == .command)
    #expect(decoded.type == "session.send")
    #expect(decoded.id == "1")
    let body = decoded.decodeBody(SessionSendBody.self)
    #expect(body?.text == "hi")
}

@Test
func decodeServerTextEvent() throws {
    // A line exactly as the Go server would emit it.
    let raw = #"{"v":1,"kind":"event","type":"session.text","ts":123,"body":{"sessionKey":"u1","delta":"hello"}}"#
    let env = try ControlCodec.decode(Data(raw.utf8))
    #expect(env.kind == .event)
    let body = env.decodeBody(SessionTextBody.self)
    #expect(body?.delta == "hello")
}

@Test
func lineFramerSplitsAndBuffersPartials() throws {
    let framer = LineFramer()
    // two full lines + a partial
    let lines1 = try framer.push(Data(#"{"a":1}"#.utf8) + Data([0x0A]) + Data(#"{"b":2}"#.utf8) + Data([0x0A]) + Data(#"{"c":"#.utf8))
    #expect(lines1.count == 2)
    // completing the partial yields the third
    let lines2 = try framer.push(Data("3}\n".utf8))
    #expect(lines2.count == 1)
    #expect(String(data: lines2[0], encoding: .utf8) == #"{"c":3}"#)
}

@Test
func appStateAccumulatesTextAndReply() throws {
    var state = AppState()
    func event(_ type: String, _ json: String) -> Envelope {
        try! ControlCodec.decode(Data(#"{"kind":"event","type":""#.utf8) + Data(type.utf8)
            + Data(#"","body":"#.utf8) + Data(json.utf8) + Data("}".utf8))
    }
    state.apply(event("session.activity", #"{"sessionKey":"u1","kind":"turnStart"}"#))
    state.apply(event("session.text", #"{"sessionKey":"u1","delta":"hel"}"#))
    state.apply(event("session.text", #"{"sessionKey":"u1","delta":"lo"}"#))
    state.apply(event("session.tool", #"{"sessionKey":"u1","name":"Bash","params":"ls"}"#))
    state.apply(event("session.usage", #"{"sessionKey":"u1","inputTokens":10,"outputTokens":3}"#))
    state.apply(event("session.reply", #"{"sessionKey":"u1","text":"hello"}"#))

    let s = state.bots["default"]?.sessions["u1"]
    #expect(s?.streamingText == "hello")
    #expect(s?.lastReply == "hello")
    #expect(s?.lastTool == "Bash")
    #expect(s?.outputTokens == 3)
}

@Test
func appStateTurnStartResetsStreamingBuffer() throws {
    var state = AppState()
    func event(_ type: String, _ json: String) -> Envelope {
        try! ControlCodec.decode(Data(#"{"kind":"event","type":""#.utf8) + Data(type.utf8)
            + Data(#"","body":"#.utf8) + Data(json.utf8) + Data("}".utf8))
    }
    state.apply(event("session.text", #"{"sessionKey":"u1","delta":"old turn"}"#))
    state.apply(event("session.activity", #"{"sessionKey":"u1","kind":"turnStart"}"#))
    #expect(state.bots["default"]?.sessions["u1"]?.streamingText == "")
}

@Test
func appStateBucketsByBotID() throws {
    var state = AppState()
    func event(_ type: String, _ json: String) -> Envelope {
        try! ControlCodec.decode(Data(#"{"kind":"event","type":""#.utf8) + Data(type.utf8)
            + Data(#"","body":"#.utf8) + Data(json.utf8) + Data("}".utf8))
    }
    state.apply(event("session.text", #"{"botId":"alpha","sessionKey":"u1","delta":"A"}"#))
    state.apply(event("session.text", #"{"botId":"beta","sessionKey":"u1","delta":"B"}"#))
    #expect(state.bots["alpha"]?.sessions["u1"]?.streamingText == "A")
    #expect(state.bots["beta"]?.sessions["u1"]?.streamingText == "B")
    #expect(state.sortedBots.map { $0.id } == ["alpha", "beta"])
}

@Test
func appStateSetBotsPreservesSessions() throws {
    var state = AppState()
    func event(_ type: String, _ json: String) -> Envelope {
        try! ControlCodec.decode(Data(#"{"kind":"event","type":""#.utf8) + Data(type.utf8)
            + Data(#"","body":"#.utf8) + Data(json.utf8) + Data("}".utf8))
    }
    state.apply(event("session.text", #"{"botId":"alpha","sessionKey":"u1","delta":"hi"}"#))
    state.setBots([BotInfo(id: "alpha", connected: true, lastError: nil)])
    let a = state.bots["alpha"]
    #expect(a?.connected == true)
    #expect(a?.sessions["u1"]?.streamingText == "hi") // session not clobbered
}

@Test
func appStateBotStatusEventUpdatesConnection() throws {
    var state = AppState()
    func event(_ type: String, _ json: String) -> Envelope {
        try! ControlCodec.decode(Data(#"{"kind":"event","type":""#.utf8) + Data(type.utf8)
            + Data(#"","body":"#.utf8) + Data(json.utf8) + Data("}".utf8))
    }
    state.apply(event("bot.status", #"{"id":"alpha","connected":true}"#))
    #expect(state.bots["alpha"]?.connected == true)
    state.apply(event("bot.status", #"{"id":"alpha","connected":false,"lastError":"dropped"}"#))
    #expect(state.bots["alpha"]?.connected == false)
    #expect(state.bots["alpha"]?.lastError == "dropped")
}

@Test
func appStateErrorEventBucketsToDefaultBot() throws {
    var state = AppState()
    func event(_ type: String, _ json: String) -> Envelope {
        try! ControlCodec.decode(Data(#"{"kind":"event","type":""#.utf8) + Data(type.utf8)
            + Data(#"","body":"#.utf8) + Data(json.utf8) + Data("}".utf8))
    }
    // No botId → the "default" bucket carries the error.
    state.apply(event("error", #"{"scope":"gateway","message":"boom","recoverable":false}"#))
    #expect(state.bots["default"]?.lastError == "boom")
    // With a botId, it lands on that bot.
    state.apply(event("error", #"{"botId":"beta","scope":"driver","message":"beta boom"}"#))
    #expect(state.bots["beta"]?.lastError == "beta boom")
}

@Test
func appStateIgnoresNonEventEnvelopes() throws {
    var state = AppState()
    // A response envelope must not mutate session state.
    let resp = try ControlCodec.decode(Data(#"{"kind":"response","type":"session.text","body":{"sessionKey":"u1","delta":"x"}}"#.utf8))
    state.apply(resp)
    #expect(state.bots.isEmpty)
}
