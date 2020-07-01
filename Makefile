NAME ?= rsync-proxy
VERSION ?= $(shell git describe --tags || echo "unknown")
BUILD_DATE := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
GIT_COMMIT := $(shell git rev-parse HEAD)
GO_LDFLAGS='-X "github.com/ustclug/rsync-proxy/cmd.Version=$(VERSION)" \
	-X "github.com/ustclug/rsync-proxy/cmd.BuildDate=$(BUILD_DATE)" \
	-X "github.com/ustclug/rsync-proxy/cmd.GitCommit=$(GIT_COMMIT)" \
	-w -s'
GOBUILD=CGO_ENABLED=0 go build -trimpath -ldflags $(GO_LDFLAGS)
PLATFORM_LIST = darwin-amd64 linux-amd64

darwin-amd64:
	GOARCH=amd64 GOOS=darwin $(GOBUILD) -o $(NAME)-$(VERSION)-$@/$(NAME)

linux-amd64:
	GOARCH=amd64 GOOS=linux $(GOBUILD) -o $(NAME)-$(VERSION)-$@/$(NAME)

all-arch: $(PLATFORM_LIST)

gz_releases=$(addsuffix .tar.gz, $(PLATFORM_LIST))

$(gz_releases): %.tar.gz : %
	tar czf $(NAME)-$(VERSION)-$@ $(NAME)-$(VERSION)-$</

releases: $(gz_releases)

clean:
	rm -rf $(NAME)-*
