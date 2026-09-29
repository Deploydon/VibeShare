.PHONY: help router swift build app universal dmg universal-dmg install run clean test fetch-provider littlesnitch

GO ?= $(shell command -v go || echo /opt/homebrew/bin/go)
SWIFT_SDK ?= $(shell sdk=$$(xcrun --show-sdk-path 2>/dev/null); if [ -n "$$sdk" ] && [ -d /Library/Developer/CommandLineTools/SDKs/MacOSX26.5.sdk ] && realpath "$$sdk" | grep -q MacOSX27; then echo /Library/Developer/CommandLineTools/SDKs/MacOSX26.5.sdk; fi)

help: ## Show this help
	@echo "VibeShare — share your AI subscriptions with friends, peer-to-peer"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

fetch-provider: ## Download the cli-proxy-api provider binary into Resources
	@./scripts/fetch-cliproxyapi.sh

router: ## Build the Go router into Swift Resources
	@echo "🔨 Building vibeshare-router..."
	@rm -f src/Sources/Resources/vibeshare-router # go build won't overwrite a fat (universal) binary
	@cd router && $(GO) build -trimpath -o ../src/Sources/Resources/vibeshare-router .
	@echo "✅ src/Sources/Resources/vibeshare-router"

cli: ## Build the vibeshare agent CLI into Swift Resources
	@echo "🔨 Building vibeshare CLI..."
	@rm -f src/Sources/Resources/vibeshare
	@cd router && $(GO) build -trimpath -o ../src/Sources/Resources/vibeshare ./cmd/vibeshare
	@echo "✅ src/Sources/Resources/vibeshare"

swift: router cli ## Build the Swift app (debug)
	@cd src && swift build $(if $(SWIFT_SDK),--sdk "$(SWIFT_SDK)",)
	@echo "✅ src/.build/debug/VibeShare"

build: swift ## Alias for swift (debug build)

app: ## Build and assemble VibeShare.app (release, signed, this machine's arch)
	@./create-app-bundle.sh

universal: ## Build a universal VibeShare.app (runs on Intel AND Apple Silicon)
	@UNIVERSAL=1 ./create-app-bundle.sh

dmg: ## Build VibeShare.app and package a distributable VibeShare-<version>.dmg
	@./create-dmg.sh

universal-dmg: ## Package a universal VibeShare.app into a .dmg (Intel + Apple Silicon)
	@UNIVERSAL=1 ./create-dmg.sh

install: app ## Build and install to /Applications
	@if pgrep -x VibeShare >/dev/null; then echo "Quit the running VibeShare app before installing a new signed build."; exit 1; fi
	@rm -rf "/Applications/VibeShare.app"
	@cp -R "VibeShare.app" /Applications/
	@echo "✅ Installed /Applications/VibeShare.app"
	@./scripts/littlesnitch-sync.sh /Applications/VibeShare.app

littlesnitch: ## Stop Little Snitch from blocking the installed app (no identity pinning)
	@./scripts/littlesnitch-sync.sh /Applications/VibeShare.app

run: install ## Build, replace the installed app, and launch it
	@open "/Applications/VibeShare.app"

test: ## Test the Go router
	@cd router && $(GO) test ./...
	@echo "✅ go tests clean"

clean: ## Remove build artifacts and bundled router binary
	@rm -rf src/.build router/vibeshare-router src/Sources/Resources/vibeshare-router src/Sources/Resources/vibeshare "VibeShare.app" VibeShare-*.dmg
	@echo "✅ cleaned"
