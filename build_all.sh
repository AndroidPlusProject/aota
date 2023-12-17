#!/bin/bash

# Linux/amd64
export GOARCH=amd64
export GOOS=linux
echo "Building for ($GOOS/$GOARCH) ..."
go build -o "aota_$GOOS-$GOARCH" -ldflags "-s -w"

# MacOS/amd64
export GOARCH=amd64
export GOOS=darwin
echo "Building for ($GOOS/$GOARCH) ..."
go build -o "aota_$GOOS-$GOARCH" -ldflags "-s -w"

# Windows/amd64
export GOARCH=amd64
export GOOS=windows
echo "Building for ($GOOS/$GOARCH) ..."
go build -o "aota_$GOOS-$GOARCH.exe" -ldflags "-s -w"

#echo "Installing targets ..."
#cp aota_* ~/bin/

echo "Installing target ..."
go install -ldflags="-s -w"

echo "Migrating targets to www ..."
mv aota_* /var/www/files/aota/

echo "- Finished building all targets!"
