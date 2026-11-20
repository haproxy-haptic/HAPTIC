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

package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Replicas share the release's mounted webhook keypair. During certificate
// rotation replicas with different leaves temporarily cannot forward.
func (s *Server) handlePeer(w http.ResponseWriter, r *http.Request) {
	cert, err := s.getCertificate(nil)
	if err != nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 ||
		!bytes.Equal(r.TLS.PeerCertificates[0].Raw, cert.Certificate[0]) {
		http.Error(w, "controller identity required", http.StatusForbidden)
		return
	}
	s.handleValidation(w, r)
}

// ForwardAdmission sends one full review directly to one replica. A dedicated
// path also makes an older replica fail closed during a rolling upgrade.
func (s *Server) ForwardAdmission(ctx context.Context, address, serviceDNS string, request *admissionv1.AdmissionRequest) (*admissionv1.AdmissionResponse, error) {
	transport, err := s.peerTransport(serviceDNS)
	if err != nil {
		return nil, err
	}
	defer transport.CloseIdleConnections()
	review := admissionv1.AdmissionReview{TypeMeta: metav1.TypeMeta{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "AdmissionReview"}, Request: request}
	body, err := json.Marshal(review)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+address+s.config.Path+"/peer", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer HTTP status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAdmissionReviewBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxAdmissionReviewBytes {
		return nil, errors.New("oversized peer review")
	}
	var answer admissionv1.AdmissionReview
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	if answer.TypeMeta != review.TypeMeta || answer.Request != nil || answer.Response == nil || answer.Response.UID != request.UID {
		return nil, errors.New("peer response does not match the admission request")
	}
	r := answer.Response
	if (!r.Allowed && (r.Result == nil || r.Result.Code != http.StatusForbidden)) || (r.Allowed && r.Result != nil) {
		return nil, errors.New("peer did not return a conclusive admission verdict")
	}
	return r, nil
}

func (s *Server) peerTransport(serviceDNS string) (*http.Transport, error) {
	cert, err := s.getCertificate(nil)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      roots,
		ServerName:   serviceDNS,
		Certificates: []tls.Certificate{*cert},
		VerifyConnection: func(state tls.ConnectionState) error {
			if !bytes.Equal(state.PeerCertificates[0].Raw, leaf.Raw) {
				return errors.New("peer webhook identity differs")
			}
			return nil
		},
	}}, nil
}
