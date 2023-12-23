#!/bin/bash

mkdir out
export PREFIX="out/aota_"

# Linux/amd64
export GOARCH=amd64
export GOOS=linux
echo "Building for ($GOOS/$GOARCH) ..."
go build -o "$PREFIX$GOOS-$GOARCH" -ldflags "-s -w"

# MacOS/amd64
export GOARCH=amd64
export GOOS=darwin
echo "Building for ($GOOS/$GOARCH) ..."
go build -o "$PREFIX$GOOS-$GOARCH" -ldflags "-s -w"

# Windows/amd64
export GOARCH=amd64
export GOOS=windows
echo "Building for ($GOOS/$GOARCH) ..."
go build -o "$PREFIX$GOOS-$GOARCH.exe" -ldflags "-s -w"

unset GOARCH
unset GOOS
echo "Installing target ..."
go install -ldflags="-s -w"

echo "- Finished building all targets!"
