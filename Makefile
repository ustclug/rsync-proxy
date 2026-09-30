GORELEASER ?= goreleaser
OUTDIR := dist

.PHONY: all linux-amd64 release clean

all:
	$(GORELEASER) build --snapshot --clean

linux-amd64:
	GOOS=linux GOARCH=amd64 $(GORELEASER) build --single-target --snapshot --clean

release:
	$(GORELEASER) release --snapshot --clean

clean:
	rm -rf $(OUTDIR)/
