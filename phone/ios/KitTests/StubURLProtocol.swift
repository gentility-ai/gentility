import Foundation

/// Answers every request a test session makes, and remembers them. A 3xx
/// answer is a redirect to `elsewhere.test`, the same path, as a server's
/// would be; a `fails` error stands for no answer at all.
final class StubURLProtocol: URLProtocol {
    struct Seen {
        let request: URLRequest
        let body: Data
    }

    nonisolated(unsafe) static var answer: (URLRequest) -> (Int, String) = { _ in (500, "{}") }
    nonisolated(unsafe) static var fails: URLError?
    nonisolated(unsafe) static var seen: [Seen] = []

    static func configuration() -> URLSessionConfiguration {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [StubURLProtocol.self]
        return configuration
    }

    static func session() -> URLSession {
        URLSession(configuration: configuration())
    }

    static func reset() {
        answer = { _ in (500, "{}") }
        fails = nil
        seen = []
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let body = Self.body(of: request)
        Self.seen.append(Seen(request: request, body: body))
        if let failure = Self.fails {
            client?.urlProtocol(self, didFailWithError: failure)
            return
        }
        let (status, text) = Self.answer(request)
        var headers = ["Content-Type": "application/json"]
        if (300..<400).contains(status) {
            let elsewhere = URL(string: "https://elsewhere.test\(request.url!.path)")!
            headers["Location"] = elsewhere.absoluteString
            var onward = URLRequest(url: elsewhere)
            onward.httpMethod = request.httpMethod
            onward.httpBody = body
            onward.allHTTPHeaderFields = request.allHTTPHeaderFields
            let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: headers)!
            client?.urlProtocol(self, wasRedirectedTo: onward, redirectResponse: response)
        }
        let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: headers)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(text.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}

    /// URLSession hands a protocol its body as a stream.
    private static func body(of request: URLRequest) -> Data {
        if let body = request.httpBody { return body }
        guard let stream = request.httpBodyStream else { return Data() }
        stream.open()
        defer { stream.close() }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while stream.hasBytesAvailable {
            let read = stream.read(&buffer, maxLength: buffer.count)
            if read <= 0 { break }
            data.append(buffer, count: read)
        }
        return data
    }
}
