import Foundation

/// What a door reply cost, by its short name.
typealias Cost = DoorConnection.Cost

/// B1: one open's timings, said as one line once the transcript is on screen.
///
/// `shown` runs from the screen's own `open()` — the tap, or the
/// notification's open — to the fold with the transcript being published.
/// Each step is from its ask to its whole reply; `decode` is the frame's
/// decode alone. `path` is the node's view of the Mac's peer once the door
/// is open; a changed path is logged again when the door closes.
///
/// Every `open()`, `retry()` and `foreground()` starts one, and a failed dial
/// clears it: only a dial that goes straight through says a line, so `in`
/// never spans a backoff or an asleep Mac's wake loop.
struct OpenReport {
    let began = ContinuousClock.now
    var dial: Duration?
    var hello: Duration?
    var transcript: Cost?
    var row: Cost?
    var shown: Duration?
    var path: PeerPath?

    var line: String? {
        guard let shown else { return nil }
        return Self.line(session: session, shown: shown, dial: dial, hello: hello, transcript: transcript, row: row, path: path)
    }

    let session: String

    init(session: String) {
        self.session = session
    }

    static func line(session: String, shown: Duration, dial: Duration?, hello: Duration?, transcript: Cost?, row: Cost?, path: PeerPath?) -> String {
        var steps: [String] = []
        if let dial { steps.append("dial \(ms(dial))") }
        if let hello { steps.append("hello \(ms(hello))") }
        if let transcript { steps.append("transcript \(ms(transcript.waited)) (\(kb(transcript.bytes)) KB, decode \(ms(transcript.decoded)))") }
        if let row { steps.append("row \(ms(row.waited)) (\(kb(row.bytes)) KB)") }
        var line = "opened \(session) in \(ms(shown)) ms"
        if !steps.isEmpty { line += " - " + steps.joined(separator: ", ") }
        if let path { line += "; path \(path.description)" }
        return line
    }

    static func ms(_ duration: Duration) -> Int {
        Int((duration / .milliseconds(1)).rounded())
    }

    static func kb(_ bytes: Int) -> Int {
        (bytes + 512) / 1024
    }
}
