// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command origin is the VM conformance suite's hermetic upstream. It answers
// on every port the guest probe tries, so a refusal the probe sees is the
// relay's or the proxy's, never a closed port: HTTPS on 443 and 8443 (any
// server name), plain HTTP on 80, an SSH-style banner on 22 and 2222, and a
// UDP echo on 53, 123 and 443.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"time"
)

func main() {
	cert, err := selfSigned()
	if err != nil {
		log.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "origin %s %s", r.Method, r.URL.Path)
	})
	for _, port := range []string{":443", ":8443"} {
		l, err := tls.Listen("tcp", port, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err != nil {
			log.Fatal(err)
		}
		go serve(l, h)
	}
	l, err := net.Listen("tcp", ":80")
	if err != nil {
		log.Fatal(err)
	}
	go serve(l, h)
	for _, port := range []string{":22", ":2222"} {
		l, err := net.Listen("tcp", port)
		if err != nil {
			log.Fatal(err)
		}
		go banner(l)
	}
	for _, port := range []string{":53", ":123", ":443"} {
		c, err := net.ListenPacket("udp", port)
		if err != nil {
			log.Fatal(err)
		}
		go echo(c)
	}
	log.Print("origin ready")
	select {}
}

func serve(l net.Listener, h http.Handler) {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	log.Fatal(srv.Serve(l))
}

func banner(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			defer c.Close()
			_, _ = io.WriteString(c, "SSH-2.0-origin\r\n")
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, _ = io.Copy(io.Discard, c)
		}()
	}
}

func echo(c net.PacketConn) {
	buf := make([]byte, 1500)
	for {
		n, addr, err := c.ReadFrom(buf)
		if err != nil {
			log.Fatal(err)
		}
		_, _ = c.WriteTo(buf[:n], addr)
	}
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "origin.test"},
		DNSNames:     []string{"origin.test", "other.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
