import XCTest
@testable import DeskPhoneKit

/// The cases of `desk/ui/src/sidebar/park.logic.test.ts`'s "the times a
/// person snoozes to", on the same clocks.
final class SnoozePresetsTests: XCTestCase {
    private let calendar: Calendar = {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "Europe/London")!
        return calendar
    }()

    private func local(_ day: Int, _ hour: Int, _ minute: Int = 0) -> Date {
        calendar.date(from: DateComponents(year: 2026, month: 9, day: day, hour: hour, minute: minute))!
    }

    private func clock(_ date: Date) -> [Int] {
        let parts = calendar.dateComponents([.day, .hour, .minute], from: date)
        return [parts.day!, parts.hour!, parts.minute!]
    }

    /// Ten in the morning, Saturday 12 September 2026.
    private var morning: Date { local(12, 10) }

    func testLabelsKeepTheirOrder() {
        XCTAssertEqual(SnoozePresets.at(morning, calendar: calendar).map(\.label), ["In an hour", "In 3 hours", "This evening", "Tomorrow morning", "Next week"])
    }

    func testAMorningOffersTheEveningToday() {
        let presets = SnoozePresets.at(morning, calendar: calendar)
        XCTAssertEqual(presets[0].until.timeIntervalSince(morning), 3600)
        XCTAssertEqual(presets[1].until.timeIntervalSince(morning), 3 * 3600)
        XCTAssertEqual(presets.map { clock($0.until) }, [[12, 11, 0], [12, 13, 0], [12, 18, 0], [13, 9, 0], [14, 9, 0]])
        XCTAssertEqual(presets.map(\.when), ["11:00", "13:00", "18:00", "Sun 09:00", "Mon 09:00"])
    }

    func testTheEveningRollsToTomorrowOncePastOrAtSix() {
        XCTAssertEqual(clock(SnoozePresets.at(local(12, 20), calendar: calendar)[2].until), [13, 18, 0])
        XCTAssertEqual(clock(SnoozePresets.at(local(12, 18), calendar: calendar)[2].until), [13, 18, 0])
    }

    func testWithinThreeHoursOfSixTheEveningIsTomorrows() {
        let presets = SnoozePresets.at(local(12, 15), calendar: calendar)
        XCTAssertEqual(clock(presets[1].until), [12, 18, 0])
        XCTAssertEqual(clock(presets[2].until), [13, 18, 0])
        XCTAssertEqual(presets[2].when, "Sun 18:00")
        XCTAssertEqual(clock(SnoozePresets.at(local(12, 14, 59), calendar: calendar)[2].until), [12, 18, 0])
    }

    func testOnAMondayNextWeekIsTheMondayAfter() {
        XCTAssertEqual(clock(SnoozePresets.at(local(14, 10), calendar: calendar)[4].until), [21, 9, 0])
    }

    func testOnSundayNextWeekIsTheMondayAfterNextAndDated() {
        let presets = SnoozePresets.at(local(13, 10), calendar: calendar)
        XCTAssertEqual(clock(presets[3].until), [14, 9, 0])
        XCTAssertEqual(clock(presets[4].until), [21, 9, 0])
        XCTAssertEqual(presets[3].when, "Mon 09:00")
        XCTAssertEqual(presets[4].when, "Mon 21 Sep 09:00")
    }
}
