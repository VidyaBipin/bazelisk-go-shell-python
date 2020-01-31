#!/bin/bash
#
# Copyright 2019 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euxo pipefail

### Build release artifacts using Bazel.
rm -rf bazelisk bin
mkdir bin

go build
for platform in darwin linux windows; do
    ./bazelisk build --config=release \
        --platforms=@io_bazel_rules_go//go/toolchain:${platform}_amd64 \
        //:bazelisk
    if [[ $platform == windows ]]; then
        cp bazel-bin/${platform}_*/bazelisk.exe bin/bazelisk-${platform}-amd64.exe
    else
        cp bazel-bin/${platform}_*/bazelisk bin/bazelisk-${platform}-amd64
    fi
done
rm -f bazelisk

### Build release artifacts using `go build`.
# GOOS=linux GOARCH=amd64 go build -o bin/bazelisk-linux-amd64
# GOOS=darwin GOARCH=amd64 go build -o bin/bazelisk-darwin-amd64
# GOOS=windows GOARCH=amd64 go build -o bin/bazelisk-windows-amd64.exe

### Print some information about the generated binaries.
ls -lh bin/*
file bin/*

# Non-googlers: you should run this script with NPM_REGISTRY=https://registry.npmjs.org
readonly REGISTRY=${NPM_REGISTRY:-https://wombat-dressing-room.appspot.com}
# Googlers: you should npm login using the go/npm-publish service:
# $ npm login --registry https://wombat-dressing-room.appspot.com
./bazelisk run --config=release //:npm_package.publish -- --access=public --tag latest --registry $REGISTRY
