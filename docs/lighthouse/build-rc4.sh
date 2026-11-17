#!/bin/sh
set -eu
go build -p 4 -buildvcs=false -trimpath -tags netgo,osusergo,sqlite_static -ldflags '-s -w -buildid= -extldflags "-static" -X main.Version=v2.5.1-lighthouse-r1-rc4' -o /out/bifrost-http ./transports/bifrost-http
go build -p 4 -buildvcs=false -trimpath -tags netgo,osusergo,sqlite_static -ldflags '-s -w -buildid= -extldflags "-static" -X main.Version=v2.5.1-lighthouse-r1-rc4' -o /tmp/bifrost-repeat ./transports/bifrost-http
cmp /out/bifrost-http /tmp/bifrost-repeat
sha256sum /out/bifrost-http
go version -m /out/bifrost-http > /out/go-buildinfo.txt
