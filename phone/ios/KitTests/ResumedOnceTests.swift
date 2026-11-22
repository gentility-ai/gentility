import XCTest
@testable import DeskPhoneKit

final class ResumedOnceTests: XCTestCase {
    /// The phone stopped on 2026-11-22 when a lost connection answered one
    /// ping twice: the second answer is dropped.
    func testASecondAnswerIsDropped() async {
        do {
            try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
                let pong = ResumedOnce(continuation)
                pong.resume(.failure(PhoenixError.closed))
                pong.resume(.failure(PhoenixError.closed))
                pong.resume(.success(()))
            }
            XCTFail("the first answer was a failure")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
    }

    func testTheFirstAnswerIsTheOneTaken() async throws {
        let value = try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Int, Error>) in
            let once = ResumedOnce(continuation)
            once.resume(.success(1))
            once.resume(.success(2))
        }
        XCTAssertEqual(value, 1)
    }
}
