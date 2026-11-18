platform = linux/amd64
repo = harbor.cyverse.org/vice/mesa-rustgis
tag = latest
context = latest
repotag = $(repo):$(tag)
version = $(shell cat VERSION)
port = 8080
# Named docker volume holding the fixture Data Store for `make run`.
fixture = rgis-data-store
# The server is tested with the image the Dockerfile builds it with, digest included.
go_image = $(shell sed -n 's/^FROM \(golang:[^ ]*\) AS host$$/\1/p' $(context)/Dockerfile)
hadolint_image = hadolint/hadolint:v2.12.0
shellcheck_image = koalaman/shellcheck:v0.10.0

build:            ## the pins in VERSION and UPSTREAM_REF reach the build as build args
	docker buildx build --platform "$(platform)" \
		--build-arg VERSION="$(version)" \
		$(shell sed -n 's/^\([A-Z0-9_]*=.*\)/--build-arg \1/p' UPSTREAM_REF) \
		-t "$(repotag)" -t "$(repo):$(version)" --load "$(context)/"

test-server:      ## go vet + go test in a container, as root and as the image user (the host's Go is not used)
	docker run --rm -v "$(CURDIR)/latest/server:/src:ro" -w /src -e GOFLAGS=-buildvcs=false \
		"$(go_image)" sh -c 'go vet ./... && go test ./...'
	docker run --rm --user 1000 -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOFLAGS=-buildvcs=false \
		-v "$(CURDIR)/latest/server:/src:ro" -w /src "$(go_image)" go test ./...

test:             ## image smoke test: run `make build` first
	ci/test-cpu.sh "$(repotag)"

lint:
	docker run --rm -i -v "$(CURDIR)/.hadolint.yaml:/.hadolint.yaml:ro" "$(hadolint_image)" \
		hadolint --config /.hadolint.yaml - < "$(context)/Dockerfile"
	docker run --rm -v "$(CURDIR):/mnt:ro" -w /mnt "$(shellcheck_image)" \
		latest/inject.sh ci/test-cpu.sh ci/fixtures/make-data-store.sh ci/fixtures/make-range-fixtures.sh \
		scripts/upstream.sh scripts/push.sh

# The fixture is built inside the image as root and handed to uid 1000, so it works whatever the
# host uid is.
fixture:
	docker volume create "$(fixture)" >/dev/null
	docker run --rm -i --user 0 -v "$(fixture):/data-store" --entrypoint sh "$(repotag)" -s /data-store \
		< ci/fixtures/make-data-store.sh

run: fixture      ## loopback only: rgis-host has no login outside VICE
	docker run --rm --user 1000 -p "127.0.0.1:$(port):8080" \
		-e IPLANT_USER=rgistest -e REDIRECT_URL="http://localhost:$(port)" \
		-v "$(fixture):/data-store" "$(repotag)"

push:             ## :latest always; :<VERSION> only while Harbor does not have it (it is never moved)
	scripts/push.sh "$(repo)" "$(version)"

rmi:
	docker rmi "$(repotag)" "$(repo):$(version)"

.PHONY: build test-server test lint fixture run push rmi
