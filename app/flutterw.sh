#!/bin/sh
# Runs a command with the Flutter SDK inside Docker. Usage from app/ (Git Bash):
#   ./flutterw.sh 'flutter pub get && flutter analyze && flutter test'
#   ./flutterw.sh 'flutter build apk --debug --dart-define=API_BASE_URL=http://10.0.2.2:8080'
# Volumes (each filled from an official download checked against its published checksum):
#   g1flutter  /opt/flutter         Flutter 3.24.5 SDK
#   g1android  /sdk                 Android SDK (cmdline-tools, platform 34, build-tools 34) and /sdk/jdk17 (Temurin 17)
#   g1gradle   /root/.gradle        Gradle 8.3 wrapper distribution and the Maven dependency cache
#   g1pub      /root/.pub-cache     Dart packages
MSYS_NO_PATHCONV=1 exec docker run --rm -v "$(pwd -W 2>/dev/null || pwd):/app" \
  -v g1flutter:/opt -v g1pub:/root/.pub-cache -v g1android:/sdk -v g1gradle:/root/.gradle -v g1androidhome:/root/.android \
  -e ANDROID_HOME=/sdk -e JAVA_HOME=/sdk/jdk17 \
  -e PATH=/sdk/jdk17/bin:/opt/flutter/bin:/opt/flutter/bin/cache/dart-sdk/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e FLUTTER_SUPPRESS_ANALYTICS=true -e PUB_ENVIRONMENT=docker -w /app golang:1.23.4 \
  sh -c "git config --global --add safe.directory '*' && which unzip >/dev/null || (apt-get update -qq && apt-get install -y -qq unzip xz-utils >/dev/null 2>&1); $1"
