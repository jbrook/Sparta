.DEFAULT_GOAL=build
.PHONY: build test get run tags clean reset

ensure_vendor:
	mkdir -pv vendor

clean:
	rm -rf ./vendor
	go clean .

get: clean ensure_vendor
	git clone --depth=1 https://github.com/aws/aws-sdk-go ./vendor/github.com/aws/aws-sdk-go
	rm -rf ./src/main/vendor/github.com/aws/aws-sdk-go/.git
	git clone --depth=1 https://github.com/vaughan0/go-ini ./vendor/github.com/vaughan0/go-ini
	rm -rf ./src/main/vendor/github.com/vaughan0/go-ini/.git
	git clone --depth=1 https://github.com/Sirupsen/logrus ./vendor/github.com/Sirupsen/logrus
	rm -rf ./src/main/vendor/github.com/Sirupsen/logrus/.git
	git clone --depth=1 https://github.com/voxelbrain/goptions ./vendor/github.com/voxelbrain/goptions
	rm -rf ./src/main/vendor/github.com/voxelbrain/goptions/.git
	git clone --depth=1 https://github.com/mweagle/esc ./vendor/github.com/mweagle/esc
	rm -rf ./src/main/vendor/github.com/mjibson/esc/.git
	git clone --depth=1 https://github.com/tdewolff/minify ./vendor/github.com/tdewolff/minify
	rm -rf ./src/main/vendor/github.com/tdewolff/minify/.git
	git clone --depth=1 https://github.com/tdewolff/buffer ./vendor/github.com/tdewolff/buffer
	rm -rf ./src/main/vendor/github.com/tdewolff/buffer/.git

reset:
		git reset --hard
		git clean -f -d

generate:
	go generate -x

format:
	go fmt .

vet: generate
	go vet .

build: format generate vet
	GO15VENDOREXPERIMENT=1 go build .
	@echo "Build complete"

docs:
	@echo ""
	@echo "Sparta godocs: http://localhost:8090/pkg/Sparta/"
	@echo
	godoc -v -http=:8090 -index=true

test: build
	GO15VENDOREXPERIMENT=1 go test -v .

run: build
	./sparta

tags:
	gotags -tag-relative=true -R=true -sort=true -f="tags" -fields=+l .

provision: build
	go run ./applications/hello_world.go --level info provision --s3Bucket $(S3_BUCKET)

execute: build
	./sparta execute

describe: build
	rm -rf ./graph.html
	go test -v -run TestDescribe
