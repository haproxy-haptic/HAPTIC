// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build agentdocker

package agent

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/fixtures"
)

// Paths inside the pod. Under `default-path origin`, HAProxy names a map, a
// certificate and a crt-list at runtime by the literal base-relative string the
// config references — the same string the manifest and every op carry, so no
// path translation exists anywhere. The chart adds `crt-base ssl` and writes
// bare filenames into crt-lists; HAProxy still names those certificates
// `ssl/<file>`, so the crt-list ops carry the store name, not the line.
const (
	baseDir          = "/etc/haproxy"
	configPath       = "haproxy.cfg"
	hostMapPath      = "maps/host.map"
	noteMapPath      = "maps/note.map"
	crtListPath      = "ssl/crt-list.txt"
	defaultCertPath  = "ssl/default.pem"
	extraCertPath    = "ssl/extra.pem"
	defaultCertFile  = "default.pem"
	extraCertFile    = "extra.pem"
	generalFilePath  = "general/maintenance.http"
	masterSocketPath = baseDir + "/haproxy-master.sock"
	workerSocketPath = baseDir + "/haproxy-worker.sock"
)

// Ports the fixture publishes. The upstream port is internal: a frontend in the
// same process answers it, so a backend has somewhere real to send traffic.
const (
	agentPort    = 5555
	metricsPort  = 9101
	statsPort    = 8404
	httpPort     = 8080
	httpsPort    = 8443
	upstreamPort = 9000
)

// Credentials the agent reads from its environment, mirroring the chart's
// agent Secret.
const (
	agentUsername = "admin"
	agentPassword = "adminpwd"
)

// notePath answers with the note map's value for the request's Host in a
// response header, so a runtime map change is observable through real traffic.
const notePath = "/note"

// defaultsProfile is the named defaults section every rendered section inherits
// from; `add backend ... from <profile>` needs one to exist.
const defaultsProfile = "haptic-base"

// bootstrapConfig is the chart's initialConfig: /ready answers 503 until the
// controller's own configuration lands. Both sockets are present — the master
// through -S, the worker through the `global` line this MR adds to the chart.
const bootstrapConfig = `global
    log stdout len 4096 local0 info
    stats socket ` + workerSocketPath + ` mode 600 level admin
    hard-stop-after 10s

defaults
    mode http
    log global
    option httplog
    timeout connect 100
    timeout client 50000
    timeout server 50000

frontend status
    bind *:8404
    http-request return status 200 content-type text/plain string "OK" if { path /healthz }
    http-request return status 503 content-type text/plain string "Not ready - waiting for controller config" if { path /ready }

frontend http_frontend
    bind *:8080
    default_backend default_backend

backend default_backend
    http-request return status 404
`

// renderedConfig stands in for what the controller renders: /ready answers 200,
// the maps and the crt-list are loaded, and be-1 forwards to the in-process
// upstream frontend.
const renderedConfig = `global
    log stdout len 4096 local0 info
    stats socket ` + workerSocketPath + ` mode 600 level admin
    hard-stop-after 10s
    default-path origin ` + baseDir + `
    crt-base ssl

defaults ` + defaultsProfile + `
    mode http
    log global
    option httplog
    timeout connect 1s
    timeout client 30s
    timeout server 30s

frontend status from ` + defaultsProfile + `
    bind *:8404
    http-request return status 200 content-type text/plain string "OK" if { path /healthz }
    http-request return status 200 content-type text/plain string "READY" if { path /ready }

frontend upstream from ` + defaultsProfile + `
    bind *:9000
    http-request return status 200 content-type text/plain string "upstream-ok"

frontend http_frontend from ` + defaultsProfile + `
    bind *:8080
    http-request return status 200 content-type text/plain hdr x-note "%[req.hdr(host),lower,map(` + noteMapPath + `,none)]" string "noted" if { path ` + notePath + ` }
    use_backend %[req.hdr(host),lower,map(` + hostMapPath + `)]
    default_backend be-1

frontend https_frontend from ` + defaultsProfile + `
    bind *:8443 ssl crt-list ` + crtListPath + `
    default_backend be-1

backend be-1 from ` + defaultsProfile + `
    balance roundrobin
    server srv1 127.0.0.1:9000 check

backend be-2 from ` + defaultsProfile + `
    balance roundrobin
    http-request return status 200 content-type text/plain string "be-2"
`

// brokenDirective is the line that makes brokenConfig fail HAProxy's parse; the
// NACK must quote it back.
const brokenDirective = "this-is-not-a-haproxy-directive"

const brokenConfig = renderedConfig + `
backend be-broken from ` + defaultsProfile + `
    ` + brokenDirective + `
`

// hostMapContent routes by Host header; its values are backend names.
const hostMapContent = `b1.example.com be-1
b2.example.com be-2
`

// noteMapContent carries free-form values, so the suite can prove the agent
// ships values with spaces and semicolons byte-exact instead of through the
// line form, which truncates at the first space and executes past the ';'.
const noteMapContent = `a.example.com first value
b.example.com to-be-changed
c.example.com to-be-deleted
`

const generalFileContent = "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 11\r\n\r\nmaintenance"

// certificate is one PEM bundle plus what a TLS handshake should reveal about it.
type certificate struct {
	pem    string
	serial *big.Int
	common string
}

// makeCertificate builds a self-signed leaf for one SNI. The serial is the
// handle a rotation test uses: the same name served by a different certificate.
func makeCertificate(t *testing.T, commonName string, serial int64) certificate {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: commonName},
		DNSNames:              []string{commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	cert, err := fixtures.NewCertificate(template, fixtures.ECDSAP256, nil)
	if err != nil {
		t.Fatalf("create certificate for %s: %v", commonName, err)
	}
	// Keep the blank PEM separator: runtime certificate updates must preserve it.
	bundle := string(cert.PEM) + "\n" + string(cert.KeyPEM)
	return certificate{pem: bundle, serial: big.NewInt(serial), common: commonName}
}
