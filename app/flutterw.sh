#!/bin/sh
# Runs a command with the Flutter SDK inside Docker. Usage from app/ (Git Bash):
#   ./flutterw.sh 'flutter pub get && flutter analyze && flutter test'
# The SDK lives in the g1flutter volume (unpacked from the official, checksum-verified
# flutter_linux_3.24.5-stable.tar.xz); pub packages are cached in g1pub.
MSYS_NO_PATHCONV=1 exec docker run --rm -v "$(pwd -W 2>/dev/null || pwd):/app" -v g1flutter:/opt -v g1pub:/root/.pub-cache \
  -e PATH=/opt/flutter/bin:/opt/flutter/bin/cache/dart-sdk/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e FLUTTER_SUPPRESS_ANALYTICS=true -e PUB_ENVIRONMENT=docker -w /app golang:1.23.4 \
  sh -c "git config --global --add safe.directory '*' && which unzip >/dev/null || (apt-get update -qq && apt-get install -y -qq unzip xz-utils >/dev/null 2>&1); $1"
