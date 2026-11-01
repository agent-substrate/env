// Copyright 2026 Google LLC
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

package ate

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// RouterServerName is the name the atenet router's serving certificate is
// issued for.
const RouterServerName = "atenet-router.ate-system.svc"

// RouterTLSCredentials returns mTLS credentials for the atenet router's
// ingress listener, which only admits clients presenting a pod identity
// certificate whose SPIFFE ID is on its --ingress-allowed-spiffe-ids list.
// credentialBundle is a PEM file holding the client certificate chain and
// key (a podCertificate projection); caFile is the PEM trust bundle the
// router's serving certificate chains to. The credential bundle is re-read
// on every handshake so rotated pod certificates are picked up.
func RouterTLSCredentials(credentialBundle, caFile string) (credentials.TransportCredentials, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("ate: reading router CA bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ate: router CA bundle %s holds no certificates", caFile)
	}
	if _, err := loadCredentialBundle(credentialBundle); err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: RouterServerName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return loadCredentialBundle(credentialBundle)
		},
	}), nil
}

// loadCredentialBundle parses a PEM file holding a certificate chain and its
// private key.
func loadCredentialBundle(path string) (*tls.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ate: reading router credential bundle: %w", err)
	}
	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("ate: parsing router credential bundle %s", path), err)
	}
	return &cert, nil
}
