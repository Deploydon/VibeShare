#!/usr/bin/env bash
# Install a freshly built VibeShare.app as THE one copy in /Applications, then
# delete every other com.vibeshare.app bundle under $HOME (repo builds, Codex
# work copies, rollback backups) so Spotlight/Launchpad only ever show one.
set -euo pipefail

SRC="${1:-VibeShare.app}"
DEST="/Applications/VibeShare.app"
BUNDLE_ID="com.vibeshare.app"
LSREGISTER=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister

[ -d "$SRC" ] || { echo "❌ $SRC not found — run 'make app' first"; exit 1; }
if pgrep -x VibeShare >/dev/null; then
  echo "Quit the running VibeShare app before installing a new signed build."; exit 1
fi

rm -rf "$DEST"
mv "$SRC" "$DEST"   # move, not copy: no leftover build for Spotlight to index
"$LSREGISTER" -f "$DEST"
echo "✅ Installed $DEST ($(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$DEST/Contents/Info.plist"))"

# Prune stray copies: anything with our bundle id outside /Applications.
pruned=0
{
  mdfind "kMDItemCFBundleIdentifier == '$BUNDLE_ID'" 2>/dev/null
  "$LSREGISTER" -dump 2>/dev/null | sed -n 's/^path: *\(.*\.app\) (0x[0-9a-f]*)$/\1/p' | grep -i vibeshare
} | sort -u | while IFS= read -r app; do
  [ "$app" = "$DEST" ] && continue
  case "$app" in *.app) ;; *) continue ;; esac   # only bundles, never data dirs
  case "$app" in "$HOME"/*) ;; *) continue ;; esac   # never touch /Volumes (DMGs) etc.
  case "$app" in "$HOME"/.Trash/*) "$LSREGISTER" -u "$app" 2>/dev/null || true; continue ;; esac
  [ -d "$app" ] || { "$LSREGISTER" -u "$app" 2>/dev/null || true; continue; }
  id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist" 2>/dev/null || true)
  [ "$id" = "$BUNDLE_ID" ] || continue
  "$LSREGISTER" -u "$app" 2>/dev/null || true
  rm -rf "$app"
  echo "🧹 removed stray copy: $app"
done
