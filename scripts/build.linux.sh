set -e  # exit on error

rm -rf /tmp/sling
cp -r . /tmp/sling

cd /tmp/sling
rm -rf .git

mkdir python/sling/bin
GOOS=linux GOARCH=amd64 go build -o sling-linux cmd/sling/*.go
mv -f sling-linux python/sling/bin/

rm -rf /tmp/sling
