# Phone app

The iOS app uses Xcode and XcodeGen. Its Go framework is built from the
Connector in `connector/` by `build-phonenode.sh`.

## Signing and a device build

Create `ios/Signing.local.xcconfig` with your Apple development team's value:

```xcconfig
DEVELOPMENT_TEAM = YOUR_TEAM_ID
```

That file is git-ignored and optional for simulator builds. Get your team ID
from your Apple developer account. Keep it and device identifiers out of
tracked files.

Build the framework with `./phone/build-phonenode.sh`. List connected
devices with `xcrun devicectl list devices`, then run
`./phone/ios/run-device.sh <device-id>`. The script generates the Xcode
project, builds, installs and launches the phone app on that device.

## Kit tests

From `phone/ios`, run `xcodegen generate`, then:

```sh
xcodebuild test -project DeskPhone.xcodeproj -scheme DeskPhoneKit \
  -destination 'platform=iOS Simulator,name=iPhone 17 Pro,OS=26.5' \
  -derivedDataPath ../build/kit-tests CODE_SIGN_IDENTITY=- DEVELOPMENT_TEAM=
```

Choose an installed model and OS from `xcrun simctl list devices available`.
Pin both: Xcode otherwise selects the latest OS, which may lack that model.
Keep ad-hoc signing enabled: keychain tests need signed simulator entitlements,
not an Apple development team. KitTests
use the test host and recorded protocol; they do not need the Go framework
or a running server.
