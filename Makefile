.PHONY: build probe sign install-hooks test

build:
	CGO_ENABLED=1 go build -o bin/cm2d ./cmd/cm2d
	CGO_ENABLED=1 go build -o bin/cm2probe ./cmd/cm2probe

probe:
	CGO_ENABLED=1 go build -o bin/cm2probe ./cmd/cm2probe

# The stable self-signed signing cert ("cm2-claude-signing" in the login
# keychain) is created once by hand via Keychain Access, per docs/design.md.
# Until that cert exists, codesign will fail here — that failure is expected
# and should surface, not be swallowed.
sign:
	codesign -f -s "$${CM2_SIGN_IDENTITY:-cm2-claude-signing}" bin/cm2d

install-hooks:
	@echo "TODO: implemented in bead ai-6ky.6"
	@exit 1

test:
	go test ./...
