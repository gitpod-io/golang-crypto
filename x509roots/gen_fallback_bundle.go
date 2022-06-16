// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build generate

//go:generate go run gen_fallback_bundle.go

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/pem"
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"sort"

	"github.com/gitpod-io/golang-crypto/x509roots/nss"
)

const tmpl = `// Code generated by gen_fallback_bundle.go; DO NOT EDIT.

//go:build go1.20

package fallback

import "crypto/x509"
import "encoding/pem"

func mustParse(b []byte) []*x509.Certificate {
	var roots []*x509.Certificate
	for len(b) > 0 {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			panic("unexpected PEM block type: " + block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			panic(err)
		}
		roots = append(roots, cert)
	}
	return roots
}

var bundle = mustParse([]byte(pemRoots))

// Format of the PEM list is:
//   * Subject common name
//   * SHA256 hash
//   * PEM block

`

var (
	certDataURL  = flag.String("certdata-url", "https://hg.mozilla.org/mozilla-central/raw-file/tip/security/nss/lib/ckfw/builtins/certdata.txt", "URL to the raw certdata.txt file to parse (certdata-path overrides this, if provided)")
	certDataPath = flag.String("certdata-path", "", "Path to the NSS certdata.txt file to parse (this overrides certdata-url, if provided)")
	output       = flag.String("output", "fallback/bundle.go", "Path to file to write output to")
)

func main() {
	flag.Parse()

	var certdata io.Reader

	if *certDataPath != "" {
		f, err := os.Open(*certDataPath)
		if err != nil {
			log.Fatalf("unable to open %q: %s", *certDataPath, err)
		}
		defer f.Close()
		certdata = f
	} else {
		resp, err := http.Get(*certDataURL)
		if err != nil {
			log.Fatalf("failed to request %q: %s", *certDataURL, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			log.Fatalf("got non-200 OK status code: %v body: %q", resp.Status, body)
		} else if ct, want := resp.Header.Get("Content-Type"), `text/plain; charset="UTF-8"`; ct != want {
			if mediaType, _, err := mime.ParseMediaType(ct); err != nil {
				log.Fatalf("bad Content-Type header %q: %v", ct, err)
			} else if mediaType != "text/plain" {
				log.Fatalf("got media type %q, want %q", mediaType, "text/plain")
			}
		}
		certdata = resp.Body
	}

	certs, err := nss.Parse(certdata)
	if err != nil {
		log.Fatalf("failed to parse %q: %s", *certDataPath, err)
	}

	if len(certs) == 0 {
		log.Fatal("certdata.txt appears to contain zero roots")
	}

	sort.Slice(certs, func(i, j int) bool {
		// Sort based on the stringified subject (which may not be unique), and
		// break any ties by just sorting on the raw DER (which will be unique,
		// but is expensive). This should produce a stable sorting, which should
		// be mostly readable by a human looking for a specific root or set of
		// roots.
		subjI, subjJ := certs[i].X509.Subject.String(), certs[j].X509.Subject.String()
		if subjI == subjJ {
			return string(certs[i].X509.Raw) < string(certs[j].X509.Raw)
		}
		return subjI < subjJ
	})

	b := new(bytes.Buffer)
	b.WriteString(tmpl)
	fmt.Fprintln(b, "const pemRoots = `")
	for _, c := range certs {
		if len(c.Constraints) > 0 {
			// Until the constrained roots API lands, skip anything that has any
			// additional constraints. Once that API is available, we can add
			// build constraints that support both the current version and the
			// new version.
			continue
		}
		fmt.Fprintf(b, "# %s\n# %x\n", c.X509.Subject.String(), sha256.Sum256(c.X509.Raw))
		pem.Encode(b, &pem.Block{Type: "CERTIFICATE", Bytes: c.X509.Raw})
	}
	fmt.Fprintln(b, "`")

	formatted, err := format.Source(b.Bytes())
	if err != nil {
		log.Fatalf("failed to format source: %s", err)
	}

	if err := os.WriteFile(*output, formatted, 0644); err != nil {
		log.Fatalf("failed to write to %q: %s", *output, err)
	}
}
