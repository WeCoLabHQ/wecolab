# WeCoLab for Mac. `make dmg` builds, signs and packages; `make test` runs the unit tests.
CONFIG ?= release
BIN = $(shell swift build -c $(CONFIG) --show-bin-path)
APP := dist/WeCoLab.app

.PHONY: build sign dmg test clean

# The app bundle is assembled by hand: an executable, an Info.plist and the icons.
build:
	swift build -c $(CONFIG)
	rm -rf $(APP) && mkdir -p $(APP)/Contents/MacOS
	cp Info.plist $(APP)/Contents/
	mkdir -p $(APP)/Contents/Resources && cp AppIcon.icns $(APP)/Contents/Resources/
	# The native icon (macOS 26 and later draw legacy icons small, on a gray tile): Icon Composer
	# source compiled into Assets.car by actool, which ships with Xcode. Without Xcode the .icns stays.
	if xcrun --find actool >/dev/null 2>&1; then xcrun actool --compile $(APP)/Contents/Resources --platform macosx \
	  --minimum-deployment-target 14.0 --app-icon AppIcon --output-partial-info-plist dist/icon-info.plist AppIcon.icon >/dev/null \
	  && rm -f dist/icon-info.plist; fi
	cp $(BIN)/WeCoLab $(APP)/Contents/MacOS/
	rm -f dist/wecolab-node && cp $(BIN)/wecolab-node dist/

# Ad-hoc signature with the virtualization entitlement, which Virtualization.framework requires.
sign: build
	codesign --force --sign - --entitlements WeCoLab.entitlements dist/wecolab-node
	codesign --force --sign - --entitlements WeCoLab.entitlements $(APP)
	codesign --verify --strict $(APP)

dmg: sign
	rm -rf dist/dmg dist/WeCoLab.dmg && mkdir dist/dmg
	cp -R $(APP) dist/dmg/ && ln -s /Applications dist/dmg/Applications
	hdiutil create -quiet -volname WeCoLab -srcfolder dist/dmg -format UDZO dist/WeCoLab.dmg
	rm -rf dist/dmg

test:
	swift test

clean:
	rm -rf .build dist
