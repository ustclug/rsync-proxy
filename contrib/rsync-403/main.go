package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"time"
)

const defaultTimeout = 10 * time.Second
const motdTemplate = `USTC Mirrors has denied your access due to inappropriate Rsync usage.
See our help page for more information: <https://mirrors.ustc.edu.cn/help/rsync-guide.html>
@ERROR: Access denied for %s
`

var (
	// Daemon auth list is a must in server version since 32.0
	// See https://github.com/RsyncProject/rsync/blob/a6312e60c95e5ebb5764eaf18eb07be23420ebc6/clientserver.c#L203
	RsyncdServerVersion = []byte("@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n")
)

func handle(conn net.Conn) error {
	go io.Copy(io.Discard, conn)
	defer conn.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.Write(RsyncdServerVersion)
	_, _ = fmt.Fprintf(buf, motdTemplate, conn.RemoteAddr().String())

	_ = conn.SetWriteDeadline(time.Now().Add(defaultTimeout))
	_, err := conn.Write(buf.Bytes())
	return err
}

func main() {
	var listenAddr string
	flag.StringVar(&listenAddr, "l", ":872", "listen address")
	flag.Parse()

	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		panic(err)
	}
	defer l.Close()

	for {
		c, err := l.Accept()
		if err != nil {
			panic(err)
		}
		go handle(c)
	}
}
