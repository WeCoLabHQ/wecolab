# WeCoLab for Mac. Development signing does not install or enable a network firewall.
CONFIG ?= release
SIGN_IDENTITY ?= -
BIN := $(shell swift build -c $(CONFIG) --show-bin-path)
APP := dist/WeCoLab.app
HELPERS := wecolab-runner wecolab-vm wecolab-network

.PHONY: build sign dmg test test-sandbox clean

# The app bundle is assembled by hand: an executable, an Info.plist and the icons.
build:
	swift build -c $(CONFIG)
	go -C network build -trimpath -o "$(BIN)/wecolab-network" .
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
	for helper in $(HELPERS); do cp "$(BIN)/$$helper" dist/; cp "$(BIN)/$$helper" $(APP)/Contents/MacOS/; done

# Only the VM worker receives filesystem/network capabilities and virtualization privileges.
# Its network child inherits that sandbox. The UI, CLI and provisioning supervisor do not host VMs.
sign: build
	for dir in dist $(APP)/Contents/MacOS; do \
	  codesign --force --sign "$(SIGN_IDENTITY)" --identifier io.wecolab.network --entitlements WeCoLabNetwork.entitlements "$$dir/wecolab-network" && \
	  codesign --force --sign "$(SIGN_IDENTITY)" --identifier io.wecolab.vm --entitlements WeCoLabVM.entitlements "$$dir/wecolab-vm" && \
	  codesign --force --sign "$(SIGN_IDENTITY)" --identifier io.wecolab.runner "$$dir/wecolab-runner" || exit 1; \
	done
	codesign --force --sign "$(SIGN_IDENTITY)" --identifier io.wecolab.cli --entitlements WeCoLab.entitlements dist/wecolab-node
	codesign --force --sign "$(SIGN_IDENTITY)" --entitlements WeCoLab.entitlements $(APP)
	codesign --verify --deep --strict $(APP)

dmg: sign
	rm -rf dist/dmg dist/WeCoLab.dmg && mkdir dist/dmg
	cp -R $(APP) dist/dmg/ && ln -s /Applications dist/dmg/Applications
	hdiutil create -quiet -volname WeCoLab -srcfolder dist/dmg -format UDZO dist/WeCoLab.dmg
	rm -rf dist/dmg

test:
	swift test
	go -C network test -race ./...

test-sandbox: sign
	WECOLAB_SIGNED_WORKER="$(abspath dist/wecolab-vm)" swift test --filter signedWorkerReceivesHomeProfileCapabilityWithoutBootingStrictVM

clean:
	rm -rf .build dist
