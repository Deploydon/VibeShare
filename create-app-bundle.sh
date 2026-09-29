#!/bin/bash
# Builds VibeShare.app: compiles the Go router + Swift menu-bar app, assembles
# the bundle, and code-signs (Developer ID if available, else ad-hoc).
set -e

GREEN='\033[0;32m'; BLUE='\033[0;34m'; YELLOW='\033[1;33m'; NC='\033[0m'

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_DIR="$PROJECT_DIR/src"
ROUTER_DIR="$PROJECT_DIR/router"
RESOURCES_DIR="$SRC_DIR/Sources/Resources"
APP_NAME="VibeShare"
EXEC_NAME="VibeShare"
APP_DIR="$PROJECT_DIR/$APP_NAME.app"

GO_BIN="$(command -v go || echo /opt/homebrew/bin/go)"
# UNIVERSAL=1 builds fat (arm64 + x86_64) binaries that run on both Apple
# Silicon and Intel Macs. Default builds for this machine's architecture only.
UNIVERSAL="${UNIVERSAL:-0}"

# The macOS 27 Command Line Tools SDK currently references SwiftUIMacros
# without shipping its plugin. The installed 26.5 SDK builds the same app on
# this host; callers may override this choice with SWIFT_SDK.
SWIFT_SDK="${SWIFT_SDK:-}"
if [ -z "$SWIFT_SDK" ] && [ -d /Library/Developer/CommandLineTools/SDKs/MacOSX26.5.sdk ]; then
  DEFAULT_SDK="$(xcrun --show-sdk-path 2>/dev/null || true)"
  if [ -n "$DEFAULT_SDK" ]; then DEFAULT_SDK="$(realpath "$DEFAULT_SDK")"; fi
  case "$DEFAULT_SDK" in
    *MacOSX27*) SWIFT_SDK=/Library/Developer/CommandLineTools/SDKs/MacOSX26.5.sdk ;;
  esac
fi
SWIFT_SDK_ARGS=()
if [ -n "$SWIFT_SDK" ]; then SWIFT_SDK_ARGS=(--sdk "$SWIFT_SDK"); fi

FETCH="$PROJECT_DIR/scripts/fetch-cliproxyapi.sh"
# 1. Provider engine binary — pinned + checksum-verified by fetch-cliproxyapi.sh.
#    Cached in Resources, so this only downloads on a fresh clone or version bump.
if [ "$UNIVERSAL" = "1" ]; then
  echo -e "${BLUE}Fetching universal cli-proxy-api (arm64 + x86_64)...${NC}"
  TMP_CPA="$(mktemp -d)"
  "$FETCH" arm64  "$TMP_CPA/cpa-arm64"
  "$FETCH" x86_64 "$TMP_CPA/cpa-amd64"
  rm -f "$RESOURCES_DIR/cli-proxy-api-plus"
  lipo -create "$TMP_CPA/cpa-arm64" "$TMP_CPA/cpa-amd64" -output "$RESOURCES_DIR/cli-proxy-api-plus"
  chmod +x "$RESOURCES_DIR/cli-proxy-api-plus"
  rm -rf "$TMP_CPA"
elif [ ! -f "$RESOURCES_DIR/cli-proxy-api-plus" ]; then
  echo -e "${BLUE}Fetching cli-proxy-api...${NC}"
  "$FETCH"
fi

# 2. Build the Go router into Resources.
echo -e "${BLUE}Building vibeshare-router (Go)...${NC}"
rm -f "$RESOURCES_DIR/vibeshare-router" # go build won't overwrite a fat (universal) binary
if [ "$UNIVERSAL" = "1" ]; then
  TMP_GO="$(mktemp -d)"
  ( cd "$ROUTER_DIR" \
      && GOOS=darwin GOARCH=arm64 "$GO_BIN" build -trimpath -o "$TMP_GO/r-arm64" . \
      && GOOS=darwin GOARCH=amd64 "$GO_BIN" build -trimpath -o "$TMP_GO/r-amd64" . )
  lipo -create "$TMP_GO/r-arm64" "$TMP_GO/r-amd64" -output "$RESOURCES_DIR/vibeshare-router"
  rm -rf "$TMP_GO"
else
  ( cd "$ROUTER_DIR" && "$GO_BIN" build -trimpath -o "$RESOURCES_DIR/vibeshare-router" . )
fi
chmod +x "$RESOURCES_DIR/vibeshare-router"
echo -e "${GREEN}✅ router archs: $(lipo -archs "$RESOURCES_DIR/vibeshare-router" 2>/dev/null)${NC}"

echo -e "${BLUE}Building vibeshare CLI (Go)...${NC}"
rm -f "$RESOURCES_DIR/vibeshare"
if [ "$UNIVERSAL" = "1" ]; then
  TMP_CLI="$(mktemp -d)"
  ( cd "$ROUTER_DIR" \
      && GOOS=darwin GOARCH=arm64 "$GO_BIN" build -trimpath -o "$TMP_CLI/c-arm64" ./cmd/vibeshare \
      && GOOS=darwin GOARCH=amd64 "$GO_BIN" build -trimpath -o "$TMP_CLI/c-amd64" ./cmd/vibeshare )
  lipo -create "$TMP_CLI/c-arm64" "$TMP_CLI/c-amd64" -output "$RESOURCES_DIR/vibeshare"
  rm -rf "$TMP_CLI"
else
  ( cd "$ROUTER_DIR" && "$GO_BIN" build -trimpath -o "$RESOURCES_DIR/vibeshare" ./cmd/vibeshare )
fi
chmod +x "$RESOURCES_DIR/vibeshare"

# 3. Build the Swift app (release).
echo -e "${BLUE}Building Swift app (release)...${NC}"
if [ "$UNIVERSAL" = "1" ]; then
  ( cd "$SRC_DIR" && swift build -c release --arch arm64 --arch x86_64 "${SWIFT_SDK_ARGS[@]}" -Xswiftc -DPACKAGED_APP )
  BUILD_DIR="$(cd "$SRC_DIR" && swift build -c release --arch arm64 --arch x86_64 "${SWIFT_SDK_ARGS[@]}" --show-bin-path)"
else
  ( cd "$SRC_DIR" && swift build -c release ${TARGET_ARCH:+--arch "$TARGET_ARCH"} "${SWIFT_SDK_ARGS[@]}" -Xswiftc -DPACKAGED_APP )
  BUILD_DIR="$(cd "$SRC_DIR" && swift build -c release ${TARGET_ARCH:+--arch "$TARGET_ARCH"} "${SWIFT_SDK_ARGS[@]}" --show-bin-path)"
fi

# 4. Assemble the .app.
echo -e "${BLUE}Assembling $APP_NAME.app...${NC}"
rm -rf "$APP_DIR"
mkdir -p "$APP_DIR/Contents/MacOS" "$APP_DIR/Contents/Resources"
cp "$BUILD_DIR/$EXEC_NAME" "$APP_DIR/Contents/MacOS/"
chmod +x "$APP_DIR/Contents/MacOS/$EXEC_NAME"

# Copy each resource (binaries, config) directly into Contents/Resources.
for item in "$RESOURCES_DIR"/*; do
  [ -e "$item" ] || continue
  case "$item" in *.swift) continue;; esac
  cp -R "$item" "$APP_DIR/Contents/Resources/"
done

cp "$SRC_DIR/Info.plist" "$APP_DIR/Contents/"
echo -n "APPL????" > "$APP_DIR/Contents/PkgInfo"

# Inject version.
VERSION="${APP_VERSION:-$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$SRC_DIR/Info.plist")}"
VERSION="${VERSION#v}"
BUILD_NUMBER="$(git -C "$PROJECT_DIR" rev-list --count HEAD 2>/dev/null || echo 1)"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString ${VERSION}" "$APP_DIR/Contents/Info.plist" 2>/dev/null || true
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion ${BUILD_NUMBER}" "$APP_DIR/Contents/Info.plist" 2>/dev/null || true

if [ ! -f "$APP_DIR/Contents/Resources/cli-proxy-api-plus" ] || \
   [ ! -f "$APP_DIR/Contents/Resources/vibeshare-router" ]; then
  echo -e "${YELLOW}⚠️  bundled binaries missing!${NC}"; exit 1
fi

# 5. Sign.
echo -e "${BLUE}Signing...${NC}"
IDENTITY="${CODESIGN_IDENTITY:-$(security find-identity -v -p codesigning 2>/dev/null | grep "Developer ID Application" | head -1 | sed 's/.*"\(.*\)"/\1/')}"
ENT="$PROJECT_DIR/entitlements.plist"
xattr -cr "$APP_DIR" 2>/dev/null || true

if [ -n "$IDENTITY" ]; then
  echo -e "${GREEN}Signing with: $IDENTITY${NC}"
  for bin in cli-proxy-api-plus vibeshare-router vibeshare; do
    [ -f "$APP_DIR/Contents/Resources/$bin" ] || continue
    codesign --force --sign "$IDENTITY" --options runtime --timestamp \
      --entitlements "$ENT" "$APP_DIR/Contents/Resources/$bin"
  done
  codesign --force --sign "$IDENTITY" --options runtime --timestamp \
    --entitlements "$ENT" "$APP_DIR/Contents/MacOS/$EXEC_NAME"
  codesign --force --sign "$IDENTITY" --options runtime --timestamp \
    --entitlements "$ENT" "$APP_DIR"
  codesign --verify --deep --strict --verbose=2 "$APP_DIR" && echo -e "${GREEN}✅ verified${NC}"
else
  echo -e "${YELLOW}No Developer ID — ad-hoc signing (fine for local use).${NC}"
  for bin in cli-proxy-api-plus vibeshare-router vibeshare; do
    [ -f "$APP_DIR/Contents/Resources/$bin" ] || continue
    codesign --force --sign - --entitlements "$ENT" "$APP_DIR/Contents/Resources/$bin" 2>/dev/null || true
  done
  codesign --force --deep --sign - --entitlements "$ENT" "$APP_DIR"
fi

echo -e "${GREEN}✅ Built $APP_DIR${NC}"
echo "   Quit VibeShare, then run: make run (replaces /Applications/VibeShare.app)"
