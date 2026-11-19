#!/usr/bin/env bash
# Applies the ZHT Aero branding and UAV engineering blueprint overlay onto a
# fetched nezha-dash user-dist directory. Idempotent and safe to re-run.

set -euo pipefail

OVERLAY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET_DIR="${1:-$(cd "$OVERLAY_DIR/.." && pwd)/user-dist}"
INDEX_HTML="$TARGET_DIR/index.html"

if [[ ! -f "$INDEX_HTML" ]]; then
  echo "error: target index.html not found at $INDEX_HTML" >&2
  exit 1
fi

cp "$OVERLAY_DIR/favicon.ico" "$TARGET_DIR/favicon.ico"
cp "$OVERLAY_DIR/zht-logo.webp" "$TARGET_DIR/zht-logo.webp"
cp "$OVERLAY_DIR/zht-uav-theme.css" "$TARGET_DIR/zht-uav-theme.css"
cp "$OVERLAY_DIR/zht-uav-decor.js" "$TARGET_DIR/zht-uav-decor.js"
rm -rf "$TARGET_DIR/aircraft"
cp -R "$OVERLAY_DIR/aircraft" "$TARGET_DIR/aircraft"

TMP_HTML="$(mktemp "${INDEX_HTML}.XXXXXX")"
trap 'rm -f "$TMP_HTML"' EXIT

awk '
  # Drop previously injected ZHT lines so repeated runs stay idempotent.
  /window\.DisableAnimatedMan[[:space:]]*=/ { next }
  /window\.CustomLogo[[:space:]]*=/ { next }
  /window\.CustomDesc[[:space:]]*=/ { skip_blank = 1; next }
  skip_blank && /^[[:space:]]*$/ { skip_blank = 0; next }
  { skip_blank = 0 }
  /zht-uav-theme\.css/ { next }
  /zht-uav-decor\.js/ { next }

  # Align first-paint theme-color and background with Obsidian Telemetry palette.
  /const themeColor = nextTheme === "dark" \?/ {
    print "          const themeColor = nextTheme === \"dark\" ? \"hsl(224 28% 5.5%)\" : \"hsl(216 33% 97.5%)\""
    next
  }
  /resolvedTheme === "dark" \? "hsl\(/ {
    print "            resolvedTheme === \"dark\" ? \"hsl(224 28% 5.5%)\" : \"hsl(216 33% 97.5%)\""
    next
  }

  # Inject runtime brand globals before theme initialization.
  /^[[:space:]]*let theme = "system"/ {
    if (!injected_globals) {
      print "        window.DisableAnimatedMan = true"
      print "        window.CustomLogo = \"/zht-logo.webp\""
      print "        window.CustomDesc = \"ZHT Aero Telemetry\""
      print ""
      injected_globals = 1
    }
    print
    next
  }

  # Replace favicon, document title, and Apple web-app title.
  /<link[^>]*rel="icon"/ {
    print "    <link rel=\"icon\" href=\"/favicon.ico?v=2\" />"
    patched_icon = 1
    next
  }
  /<title>.*<\/title>/ {
    print "    <title>中航通服务器监控</title>"
    patched_title = 1
    next
  }
  /<meta[^>]*name="apple-mobile-web-app-title"/ {
    print "    <meta name=\"apple-mobile-web-app-title\" content=\"中航通服务器监控\" />"
    next
  }

  # Inject ZHT Aero stylesheet and UAV blueprint script before </head>.
  /^[[:space:]]*<\/head>/ {
    if (!injected_assets) {
      print "    <link rel=\"stylesheet\" href=\"/zht-uav-theme.css?v=2\">"
      print "    <script defer src=\"/zht-uav-decor.js?v=2\"></script>"
      injected_assets = 1
    }
    print
    next
  }

  { print }

  END {
    if (!injected_globals || !patched_icon || !patched_title || !injected_assets) {
      print "error: failed to patch all expected anchors in index.html" > "/dev/stderr"
      exit 1
    }
  }
' "$INDEX_HTML" > "$TMP_HTML"

mv "$TMP_HTML" "$INDEX_HTML"
echo "Applied ZHT Aero user-dist overlay to $TARGET_DIR"
