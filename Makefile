# AcerviNode build — see docs/installation.md#from-source and
# docs/development.md for the full picture.
#
# The frontend must be built into web/dist before the Go binary is built:
# go:embed bakes in whatever's already on disk in web/dist at build time,
# not what's in git — web/dist's actual built files are gitignored, only a
# placeholder .gitkeep is committed (see .gitignore). A plain `git pull &&
# go build`, with no frontend step, silently succeeds and produces a real,
# runnable binary — just one still serving whatever UI happened to already
# be sitting in web/dist from an earlier build, no error, nothing to
# notice until someone actually looks at the page. Found live: exactly
# that happened updating a real deployment from source.
#
# `make build` closes that gap by always doing both steps, in the right
# order, as one command — there's no longer a multi-step sequence an
# update can partially skip.

.PHONY: build frontend build-backend-only test clean

# build is the default target (`make` with no arguments) and the one to
# actually use for a real update or release build.
build: frontend
	go build ./cmd/acervinode

frontend:
	cd web && npm ci && npm run build

# build-backend-only skips the frontend step entirely, embedding whatever
# is already in web/dist as-is (stale or not) — for the "build on a
# machine that has Node.js, copy just the resulting binary to a production
# box that deliberately doesn't" workflow. Deliberately a separate,
# explicitly-named target rather than a flag on `build`: skipping the
# frontend has to be typed on purpose, not stumbled into.
build-backend-only:
	go build ./cmd/acervinode

test:
	go vet ./...
	go test ./...

clean:
	rm -f acervinode acervinode.exe
